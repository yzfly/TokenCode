package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnthropicRequestAndParse 用假服务器离线验证：
// 1) 请求按 Anthropic 协议正确编组（headers / system / tools / tool_result 块）；
// 2) 响应里的 text 与 tool_use 块被正确解析。
func TestAnthropicRequestAndParse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotVersion string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"content": [
				{"type": "text", "text": "let me read it"},
				{"type": "tool_use", "id": "tu_1", "name": "read", "input": {"path": "a.txt"}}
			],
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 120, "output_tokens": 34}
		}`)
	}))
	defer srv.Close()

	c := NewAnthropic("sk-test", srv.URL, true)

	resp, err := c.Complete(context.Background(), Request{
		Model:     "deepseek-v4-pro[1m]",
		System:    "you are a test",
		MaxTokens: 256,
		Messages: []Message{
			{Role: RoleUser, Text: "read a.txt"},
			{Role: RoleAssistant, ToolUses: []ToolUse{{ID: "tu_0", Name: "read", Input: json.RawMessage(`{"path":"a.txt"}`)}}},
			{Role: RoleUser, ToolResults: []ToolResult{{ToolUseID: "tu_0", Content: "file body"}}},
		},
		Tools: []Tool{{Name: "read", Description: "read a file", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// --- 响应解析 ---
	if resp.Text != "let me read it" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].ID != "tu_1" || resp.ToolUses[0].Name != "read" {
		t.Fatalf("tool_use parse wrong: %+v", resp.ToolUses)
	}

	// --- 请求编组 ---
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header wrong: %q", gotAuth)
	}
	if gotVersion != anthropicVersion {
		t.Fatalf("anthropic-version wrong: %q", gotVersion)
	}
	if gotBody["model"] != "deepseek-v4-pro[1m]" {
		t.Fatalf("model wrong: %v", gotBody["model"])
	}
	if gotBody["system"] != "you are a test" {
		t.Fatalf("system wrong: %v", gotBody["system"])
	}
	if _, ok := gotBody["tools"]; !ok {
		t.Fatal("tools missing from request")
	}

	// tool_result 块要正确出现在第三条消息里。
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %v", gotBody["messages"])
	}
	third := msgs[2].(map[string]any)
	blocks := third["content"].([]any)
	blk := blocks[0].(map[string]any)
	if blk["type"] != "tool_result" || blk["tool_use_id"] != "tu_0" || blk["content"] != "file body" {
		t.Fatalf("tool_result block wrong: %v", blk)
	}
}

// TestAnthropicStopReasonNormalize 验证未知 stop_reason 归一到 StopOther。
func TestAnthropicStopReasonNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"end_turn", StopEndTurn},
		{"stop_sequence", StopEndTurn},
		{"tool_use", StopToolUse},
		{"max_tokens", StopMaxTokens},
		{"refusal", StopRefusal},
		{"pause_turn", StopOther}, // 未知值落 other
		{"", StopOther},
	}
	for _, c := range cases {
		if got := normalizeAnthropicStop(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestAnthropicHTTPError 验证非 200 返回被包成错误。
func TestAnthropicHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"auth","message":"bad key"}}`)
	}))
	defer srv.Close()

	c := NewAnthropic("x", srv.URL, false)
	if _, err := c.Complete(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("expected error on http 401")
	}
}
