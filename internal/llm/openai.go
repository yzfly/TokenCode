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

// OpenAI 是一个 OpenAI 协议（Chat Completions）客户端。
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

	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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

// CompleteStream 走 SSE 流式：逐段回调 onDelta，最终组装出与 Complete
// 相同语义的 Response。chunk 协议：choices[0].delta 携带 content /
// reasoning_content / tool_calls 片段（tool_calls 按 index 多路复用、
// arguments 分片拼接），末尾 "data: [DONE]"；用量经 stream_options.include_usage
// 在最后一个 chunk 返回（端点不支持时静默为零）。
func (c *OpenAI) CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	wire := toChatRequest(req)
	wire.Stream = true
	wire.StreamOptions = &chatStreamOptions{IncludeUsage: true}

	body, err := json.Marshal(wire)
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
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(resp.Body)
		return Response{}, fmt.Errorf("llm: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	type callAccum struct {
		id   string
		name string
		args strings.Builder
	}
	calls := map[int]*callAccum{}
	var order []int
	var text, thinking strings.Builder
	out := Response{StopReason: StopOther}

	err = readSSE(resp.Body, func(data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if ev.Error != nil {
			return fmt.Errorf("llm: %s", ev.Error.Message)
		}
		if ev.Usage != nil {
			out.Usage = Usage{InputTokens: ev.Usage.PromptTokens, OutputTokens: ev.Usage.CompletionTokens}
		}
		if len(ev.Choices) == 0 {
			return nil // 纯 usage chunk
		}
		ch := ev.Choices[0]
		if ch.Delta.Content != "" {
			text.WriteString(ch.Delta.Content)
			if onDelta != nil {
				onDelta(Delta{Text: ch.Delta.Content})
			}
		}
		if ch.Delta.ReasoningContent != "" {
			thinking.WriteString(ch.Delta.ReasoningContent)
			if onDelta != nil {
				onDelta(Delta{Thinking: ch.Delta.ReasoningContent})
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			acc := calls[tc.Index]
			if acc == nil {
				acc = &callAccum{}
				calls[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
		if ch.FinishReason != "" {
			out.StopReason = normalizeFinishReason(ch.FinishReason)
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}

	out.Text = text.String()
	out.Thinking = thinking.String()
	for _, i := range order {
		acc := calls[i]
		args := acc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		var input json.RawMessage
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			return Response{}, fmt.Errorf("llm: tool call %s arguments not valid json: %w", acc.id, err)
		}
		out.ToolUses = append(out.ToolUses, ToolUse{ID: acc.id, Name: acc.name, Input: input})
	}
	return out, nil
}
