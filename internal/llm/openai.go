package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI 是一个 OpenAI Chat Completions 协议客户端。
// 通过 baseURL 可指向任意兼容该协议的服务（DeepSeek/Kimi/Qwen/Ollama/OpenRouter 等）。
type OpenAI struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewOpenAI 创建客户端。apiKey 为空时不发鉴权头（如 Ollama 本地服务）。
func NewOpenAI(apiKey, baseURL string) *OpenAI {
	return &OpenAI{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(toChatRequest(req))
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, fmt.Errorf("llm: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var wr chatResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	return fromChatResponse(wr)
}

// ---- 线上报文形态 ----

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
	// 新旧端点对该字段命名不一，两个都发同一个值。
	MaxTokens           int `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // input 对象 marshal 成的字符串
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"` // DeepSeek/Qwen 非标字段
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// ---- IR ↔ 报文转换（纯函数，独立可测）----

// toChatRequest 把内部请求转成 Chat Completions 形态。
// 注意保序：tool 结果消息必须紧跟对应的 assistant 消息，两边端点都会对违规 400。
func toChatRequest(req Request) chatRequest {
	out := chatRequest{
		Model:               req.Model,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: req.MaxTokens,
	}

	if req.System != "" {
		s := req.System
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: &s})
	}

	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			cm := chatMessage{Role: "assistant"}
			if m.Text != "" {
				t := m.Text
				cm.Content = &t
			}
			for _, tu := range m.ToolUses {
				input := tu.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
					ID:       tu.ID,
					Type:     "function",
					Function: chatFunction{Name: tu.Name, Arguments: string(input)},
				})
			}
			if cm.Content == nil && len(cm.ToolCalls) == 0 {
				continue // 跳过空消息
			}
			out.Messages = append(out.Messages, cm)
		case RoleUser:
			// tool 结果各拆一条独立消息，紧跟在对应 assistant 之后。
			for _, tr := range m.ToolResults {
				content := tr.Content
				if tr.IsError {
					content = "Error: " + content
				}
				out.Messages = append(out.Messages, chatMessage{
					Role:       "tool",
					Content:    &content,
					ToolCallID: tr.ToolUseID,
				})
			}
			if m.Text != "" {
				t := m.Text
				out.Messages = append(out.Messages, chatMessage{Role: "user", Content: &t})
			}
		}
	}

	for _, t := range req.Tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, chatTool{
			Type:     "function",
			Function: chatToolDef{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	return out
}

// fromChatResponse 把 Chat Completions 响应解析回内部表示。
func fromChatResponse(resp chatResponse) (Response, error) {
	if len(resp.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: response has no choices")
	}
	ch := resp.Choices[0]

	out := Response{
		Text:       ch.Message.Content,
		Thinking:   ch.Message.ReasoningContent,
		StopReason: normalizeFinishReason(ch.FinishReason),
		Usage:      Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens},
	}

	for _, tc := range ch.Message.ToolCalls {
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}" // 部分端点对无参调用返回空串
		}
		var input json.RawMessage
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return Response{}, fmt.Errorf("llm: tool call %s arguments not valid json: %w", tc.ID, err)
		}
		out.ToolUses = append(out.ToolUses, ToolUse{ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	return out, nil
}

// normalizeFinishReason 把 finish_reason 归一到枚举，未知值落 StopOther。
func normalizeFinishReason(s string) StopReason {
	switch s {
	case "stop":
		return StopEndTurn
	case "tool_calls":
		return StopToolUse
	case "length":
		return StopMaxTokens
	case "content_filter":
		return StopRefusal
	default:
		return StopOther
	}
}
