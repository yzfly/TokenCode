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

// DefaultBaseURL 默认指向 DeepSeek 的 Anthropic 兼容端点。
const DefaultBaseURL = "https://api.deepseek.com/anthropic"

// DefaultModel 是默认模型。
const DefaultModel = "deepseek-v4-pro[1m]"

const anthropicVersion = "2023-06-01"

// Anthropic 是一个 Anthropic Messages API 协议客户端。
// 通过 BaseURL 可指向任意兼容该协议的服务（默认 DeepSeek）。
type Anthropic struct {
	apiKey  string
	baseURL string
	bearer  bool // true: Authorization: Bearer；false: x-api-key
	http    *http.Client
}

// NewAnthropic 创建客户端。bearer=true 用 Authorization: Bearer（对应 ANTHROPIC_AUTH_TOKEN），
// 否则用 x-api-key（对应 ANTHROPIC_API_KEY）。baseURL 为空时用 DefaultBaseURL。
func NewAnthropic(apiKey, baseURL string, bearer bool) *Anthropic {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Anthropic{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		bearer:  bearer,
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"messages":   buildMessages(req.Messages),
	}
	if req.System != "" {
		payload["system"] = req.System
	}
	if len(req.Tools) > 0 {
		payload["tools"] = buildTools(req.Tools)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if c.bearer {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else {
		httpReq.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var wr struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &wr); err != nil {
		return Response{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if wr.Error != nil {
		return Response{}, fmt.Errorf("llm: %s", wr.Error.Message)
	}

	out := Response{
		StopReason: normalizeAnthropicStop(wr.StopReason),
		Usage:      Usage{InputTokens: wr.Usage.InputTokens, OutputTokens: wr.Usage.OutputTokens},
	}
	var text strings.Builder
	for _, blk := range wr.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			out.ToolUses = append(out.ToolUses, ToolUse{ID: blk.ID, Name: blk.Name, Input: blk.Input})
		}
	}
	out.Text = text.String()
	return out, nil
}

// normalizeAnthropicStop 把端点返回的 stop_reason 归一到枚举，未知值落 StopOther。
func normalizeAnthropicStop(s string) StopReason {
	switch s {
	case "end_turn", "stop_sequence":
		return StopEndTurn
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	case "refusal":
		return StopRefusal
	default:
		return StopOther
	}
}

// buildMessages 把内部消息转成 Anthropic 的 content-block 形态。
func buildMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		var blocks []map[string]any
		if m.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": m.Text})
		}
		for _, tu := range m.ToolUses {
			input := tu.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tu.ID,
				"name":  tu.Name,
				"input": input,
			})
		}
		for _, tr := range m.ToolResults {
			blk := map[string]any{
				"type":        "tool_result",
				"tool_use_id": tr.ToolUseID,
				"content":     tr.Content,
			}
			if tr.IsError {
				blk["is_error"] = true
			}
			blocks = append(blocks, blk)
		}
		if len(blocks) == 0 {
			continue // 跳过空消息
		}
		out = append(out, map[string]any{"role": m.Role, "content": blocks})
	}
	return out
}

// buildTools 把内部工具定义转成 Anthropic 的 tools 字段。
func buildTools(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": schema,
		})
	}
	return out
}
