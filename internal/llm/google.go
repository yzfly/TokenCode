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

// DefaultGoogleBaseURL 是 Google Gemini 协议的官方端点。
const DefaultGoogleBaseURL = "https://generativelanguage.googleapis.com"

// Google 是一个 Google Gemini 协议（generateContent）客户端。
// 通过 baseURL 可指向任意兼容该协议的服务（默认 Google 官方）。
type Google struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewGoogle 创建客户端。apiKey 经 x-goog-api-key 头发送，为空时不发。
// baseURL 为空时用 DefaultGoogleBaseURL。
func NewGoogle(apiKey, baseURL string) *Google {
	if baseURL == "" {
		baseURL = DefaultGoogleBaseURL
	}
	return &Google{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Google) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(toGeminiRequest(req))
	if err != nil {
		return Response{}, err
	}

	url := c.baseURL + "/v1beta/models/" + req.Model + ":generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", c.apiKey)
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

	var wr geminiResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	return fromGeminiResponse(wr)
}

// ---- 线上报文形态 ----

type geminiRequest struct {
	SystemInstruction *geminiContent   `json:"system_instruction,omitempty"`
	Contents          []geminiContent  `json:"contents"`
	Tools             []geminiTool     `json:"tools,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"` // "user" | "model"
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	FunctionCall     *geminiFuncCall   `json:"functionCall,omitempty"`
	FunctionResponse *geminiFuncResult `json:"functionResponse,omitempty"`
}

type geminiFuncCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFuncResult struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFuncDecl `json:"functionDeclarations"`
}

type geminiFuncDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type geminiGenConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ---- IR ↔ 报文转换（纯函数，独立可测）----

// toGeminiRequest 把内部请求转成 Gemini generateContent 形态。
// Gemini 的 functionCall/functionResponse 没有调用 ID、按 name 关联，
// 所以回灌历史时用 "ID→name" 映射把 ToolResult 还原成 functionResponse；
// 映射在顺序遍历中维护，tool 结果总是紧跟其 assistant 消息，先到先解析。
func toGeminiRequest(req Request) geminiRequest {
	out := geminiRequest{}
	if req.System != "" {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: req.System}}}
	}
	if req.MaxTokens > 0 {
		out.GenerationConfig = &geminiGenConfig{MaxOutputTokens: req.MaxTokens}
	}

	idToName := map[string]string{}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleAssistant:
			gc := geminiContent{Role: "model"}
			if m.Text != "" {
				gc.Parts = append(gc.Parts, geminiPart{Text: m.Text})
			}
			for _, tu := range m.ToolUses {
				idToName[tu.ID] = tu.Name
				args := tu.Input
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				gc.Parts = append(gc.Parts, geminiPart{
					FunctionCall: &geminiFuncCall{Name: tu.Name, Args: args},
				})
			}
			if len(gc.Parts) == 0 {
				continue // 跳过空消息
			}
			out.Contents = append(out.Contents, gc)
		case RoleUser:
			gc := geminiContent{Role: "user"}
			for _, tr := range m.ToolResults {
				resp := map[string]any{"output": tr.Content}
				if tr.IsError {
					resp = map[string]any{"error": tr.Content}
				}
				gc.Parts = append(gc.Parts, geminiPart{
					FunctionResponse: &geminiFuncResult{Name: idToName[tr.ToolUseID], Response: resp},
				})
			}
			if m.Text != "" {
				gc.Parts = append(gc.Parts, geminiPart{Text: m.Text})
			}
			if len(gc.Parts) == 0 {
				continue
			}
			out.Contents = append(out.Contents, gc)
		}
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, geminiTool{
			FunctionDeclarations: []geminiFuncDecl{{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			}},
		})
	}
	return out
}

// fromGeminiResponse 把 generateContent 响应解析回内部表示。
// Gemini 不给 functionCall 发 ID，这里按 "name-序号" 合成；tool 结果回灌时
// toGeminiRequest 会按映射还原成 name，所以合成值只需会话内可关联。
func fromGeminiResponse(resp geminiResponse) (Response, error) {
	if resp.Error != nil {
		return Response{}, fmt.Errorf("llm: %s", resp.Error.Message)
	}
	if len(resp.Candidates) == 0 {
		return Response{}, fmt.Errorf("llm: response has no candidates")
	}
	cand := resp.Candidates[0]

	out := Response{
		Usage: Usage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		},
	}
	var text, thinking strings.Builder
	for i, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			args := p.FunctionCall.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.ToolUses = append(out.ToolUses, ToolUse{
				ID:    fmt.Sprintf("%s-%d", p.FunctionCall.Name, i),
				Name:  p.FunctionCall.Name,
				Input: args,
			})
		case p.Thought:
			thinking.WriteString(p.Text)
		default:
			text.WriteString(p.Text)
		}
	}
	out.Text = text.String()
	out.Thinking = thinking.String()

	// Gemini 没有 tool_use 这个 finishReason：有 functionCall 即继续调工具。
	if len(out.ToolUses) > 0 {
		out.StopReason = StopToolUse
	} else {
		out.StopReason = normalizeGeminiFinish(cand.FinishReason)
	}
	return out, nil
}

// normalizeGeminiFinish 把 finishReason 归一到枚举，未知值落 StopOther。
func normalizeGeminiFinish(s string) StopReason {
	switch s {
	case "STOP":
		return StopEndTurn
	case "MAX_TOKENS":
		return StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return StopRefusal
	default:
		return StopOther
	}
}

// CompleteStream 走 SSE 流式（:streamGenerateContent?alt=sse）：每个事件是一个
// GenerateContentResponse 形态的 chunk，文本/思考增量逐段回调，functionCall
// 与 usageMetadata 累积，最终统一经 fromGeminiResponse 组装（合成 ID 等
// 规则与非流式完全一致）。
func (c *Google) CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	body, err := json.Marshal(toGeminiRequest(req))
	if err != nil {
		return Response{}, err
	}

	url := c.baseURL + "/v1beta/models/" + req.Model + ":streamGenerateContent?alt=sse"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("x-goog-api-key", c.apiKey)
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

	// 跨 chunk 累积成一个合成响应，复用非流式的组装逻辑。
	combined := geminiResponse{}
	combined.Candidates = append(combined.Candidates, struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	}{})
	cand := &combined.Candidates[0]

	err = readSSE(resp.Body, func(data string) error {
		var chunk geminiResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("llm: %s", chunk.Error.Message)
		}
		combined.UsageMetadata = chunk.UsageMetadata // 后到的累计值覆盖
		if len(chunk.Candidates) == 0 {
			return nil
		}
		cc := chunk.Candidates[0]
		if cc.FinishReason != "" {
			cand.FinishReason = cc.FinishReason
		}
		for _, p := range cc.Content.Parts {
			cand.Content.Parts = append(cand.Content.Parts, p)
			if p.FunctionCall != nil || p.Text == "" || onDelta == nil {
				continue
			}
			if p.Thought {
				onDelta(Delta{Thinking: p.Text})
			} else {
				onDelta(Delta{Text: p.Text})
			}
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	return fromGeminiResponse(combined)
}
