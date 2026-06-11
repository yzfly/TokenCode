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

// buildPayload 组装 /v1/messages 请求体，Complete 与 CompleteStream 共用。
func (c *Anthropic) buildPayload(req Request) map[string]any {
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
	return payload
}

// post 发出请求并对非 200 统一报错（流式时调用方负责读 body）。
func (c *Anthropic) post(ctx context.Context, payload map[string]any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("llm: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return resp, nil
}

func (c *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	resp, err := c.post(ctx, c.buildPayload(req))
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

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
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
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
		Usage: Usage{
			InputTokens:      wr.Usage.InputTokens,
			OutputTokens:     wr.Usage.OutputTokens,
			CacheReadTokens:  wr.Usage.CacheReadTokens,
			CacheWriteTokens: wr.Usage.CacheCreationTokens,
		},
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

// CompleteStream 走 SSE 流式：逐段回调 onDelta，最终组装出与 Complete
// 相同语义的 Response。事件协议：message_start → content_block_start/
// content_block_delta/content_block_stop（按 index 多路复用）→
// message_delta（stop_reason + 输出用量）→ message_stop。
func (c *Anthropic) CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	payload := c.buildPayload(req)
	payload["stream"] = true

	resp, err := c.post(ctx, payload)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	type block struct {
		typ  string
		id   string
		name string
		json strings.Builder // tool_use 的 input_json_delta 拼接
		text strings.Builder // text/thinking 块正文
	}
	blocks := map[int]*block{}
	var order []int
	out := Response{StopReason: StopOther}

	err = readSSE(resp.Body, func(data string) error {
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message *struct {
				Usage struct {
					InputTokens         int `json:"input_tokens"`
					CacheReadTokens     int `json:"cache_read_input_tokens"`
					CacheCreationTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("llm: decode stream event: %w", err)
		}

		switch ev.Type {
		case "error":
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return fmt.Errorf("llm: %s", msg)
		case "message_start":
			if ev.Message != nil {
				out.Usage.InputTokens = ev.Message.Usage.InputTokens
				out.Usage.CacheReadTokens = ev.Message.Usage.CacheReadTokens
				out.Usage.CacheWriteTokens = ev.Message.Usage.CacheCreationTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil {
				blocks[ev.Index] = &block{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil || ev.Delta == nil {
				return nil
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				if onDelta != nil {
					onDelta(Delta{Text: ev.Delta.Text})
				}
			case "thinking_delta":
				b.text.WriteString(ev.Delta.Thinking)
				if onDelta != nil {
					onDelta(Delta{Thinking: ev.Delta.Thinking})
				}
			case "input_json_delta":
				b.json.WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = normalizeAnthropicStop(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}

	var text, thinking strings.Builder
	for _, i := range order {
		b := blocks[i]
		switch b.typ {
		case "text":
			text.WriteString(b.text.String())
		case "thinking":
			thinking.WriteString(b.text.String())
		case "tool_use":
			input := b.json.String()
			if strings.TrimSpace(input) == "" {
				input = "{}"
			}
			out.ToolUses = append(out.ToolUses, ToolUse{ID: b.id, Name: b.name, Input: json.RawMessage(input)})
		}
	}
	out.Text = text.String()
	out.Thinking = thinking.String()
	return out, nil
}
