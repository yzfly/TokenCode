package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseServer 起一个把固定 SSE 文本原样写回的假服务器。
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
}

// collectDeltas 返回 onDelta 回调与已收增量的取读函数。
func collectDeltas() (func(Delta), func() (text, thinking string)) {
	var text, thinking strings.Builder
	return func(d Delta) {
			text.WriteString(d.Text)
			thinking.WriteString(d.Thinking)
		}, func() (string, string) {
			return text.String(), thinking.String()
		}
}

// TestAnthropicStream 覆盖完整事件序列：text 块流式、tool_use 块的
// input_json_delta 分片拼接、message_start/message_delta 的用量与 stop_reason。
func TestAnthropicStream(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":80,"cache_creation_input_tokens":16}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"let me "}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"check"}}

data: {"type":"content_block_stop","index":0}

data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}

data: {"type":"content_block_stop","index":1}

data: {"type":"message_delta","delta":{"type":"message_delta","stop_reason":"tool_use"},"usage":{"output_tokens":34}}

data: {"type":"message_stop"}

`
	srv := sseServer(t, body)
	defer srv.Close()

	onDelta, got := collectDeltas()
	c := NewAnthropic("k", srv.URL, false)
	resp, err := c.CompleteStream(context.Background(), Request{Model: "m", MaxTokens: 64, Messages: []Message{{Role: RoleUser, Text: "hi"}}}, onDelta)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if resp.Text != "let me check" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
	if dt, _ := got(); dt != "let me check" {
		t.Fatalf("deltas wrong: %q", dt)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID != "call_1" ||
		resp.ToolUses[0].Name != "read" || string(resp.ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use wrong: %+v", resp.ToolUses)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 80 || resp.Usage.CacheWriteTokens != 16 {
		t.Fatalf("cache usage wrong: %+v", resp.Usage)
	}
}

// TestAnthropicStreamError 验证流中 error 事件被包成错误。
func TestAnthropicStreamError(t *testing.T) {
	srv := sseServer(t, "data: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n")
	defer srv.Close()

	c := NewAnthropic("k", srv.URL, false)
	if _, err := c.CompleteStream(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleUser, Text: "hi"}}}, nil); err == nil {
		t.Fatal("expected error from error event")
	}
}

// TestOpenAIStream 覆盖 content/reasoning_content 增量、tool_calls 分片
// （首片带 id/name、后片只有 arguments）、finish_reason、独立 usage chunk、[DONE]。
func TestOpenAIStream(t *testing.T) {
	body := `data: {"choices":[{"delta":{"reasoning_content":"think "},"finish_reason":""}]}

data: {"choices":[{"delta":{"content":"let me "},"finish_reason":""}]}

data: {"choices":[{"delta":{"content":"check"},"finish_reason":""}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"pa"}}]},"finish_reason":""}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]},"finish_reason":""}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":96}}}

data: [DONE]

`
	srv := sseServer(t, body)
	defer srv.Close()

	onDelta, got := collectDeltas()
	c := NewOpenAI("k", srv.URL)
	resp, err := c.CompleteStream(context.Background(), Request{Model: "m", MaxTokens: 64, Messages: []Message{{Role: RoleUser, Text: "hi"}}}, onDelta)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if resp.Text != "let me check" || resp.Thinking != "think " {
		t.Fatalf("text/thinking wrong: %q / %q", resp.Text, resp.Thinking)
	}
	if dt, dth := got(); dt != "let me check" || dth != "think " {
		t.Fatalf("deltas wrong: %q / %q", dt, dth)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID != "call_1" ||
		resp.ToolUses[0].Name != "read" || string(resp.ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use wrong: %+v", resp.ToolUses)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 96 {
		t.Fatalf("cache usage wrong: %+v", resp.Usage)
	}
}

// TestGoogleStream 覆盖 chunk 累积：thought/text 增量、整只 functionCall、
// 末 chunk 的 finishReason 与 usageMetadata，路径走 :streamGenerateContent。
func TestGoogleStream(t *testing.T) {
	body := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"think ","thought":true}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"let me "}]}}]}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":"check"},{"functionCall":{"name":"read","args":{"path":"a.txt"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":34,"cachedContentTokenCount":64}}

`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer srv.Close()

	onDelta, got := collectDeltas()
	c := NewGoogle("k", srv.URL)
	resp, err := c.CompleteStream(context.Background(), Request{Model: "gemini-2.5-flash", MaxTokens: 64, Messages: []Message{{Role: RoleUser, Text: "hi"}}}, onDelta)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("path wrong: %q", gotPath)
	}
	if resp.Text != "let me check" || resp.Thinking != "think " {
		t.Fatalf("text/thinking wrong: %q / %q", resp.Text, resp.Thinking)
	}
	if dt, dth := got(); dt != "let me check" || dth != "think " {
		t.Fatalf("deltas wrong: %q / %q", dt, dth)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read" || string(resp.ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use wrong: %+v", resp.ToolUses)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 64 {
		t.Fatalf("cache usage wrong: %+v", resp.Usage)
	}
}

// TestStreamerInterface 静态断言三个 codec 都实现 Streamer。
func TestStreamerInterface(t *testing.T) {
	var _ Streamer = (*Anthropic)(nil)
	var _ Streamer = (*OpenAI)(nil)
	var _ Streamer = (*Google)(nil)
}
