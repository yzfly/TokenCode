package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestToChatRequestGolden 用固定 IR 输入断言生成的 JSON：
// 覆盖 system 提升、tool 循环往返一整轮（user → assistant 带 tool_calls →
// tool 结果 → assistant 文本）、IsError 前缀、双 max_tokens 字段、tools 定义。
func TestToChatRequestGolden(t *testing.T) {
	req := Request{
		Model:     "kimi-k2",
		System:    "you are a test",
		MaxTokens: 256,
		Messages: []Message{
			{Role: RoleUser, Text: "read a.txt"},
			{Role: RoleAssistant, Text: "let me read it", ToolUses: []ToolUse{
				{ID: "call_0", Name: "read", Input: json.RawMessage(`{"path":"a.txt"}`)},
			}},
			{Role: RoleUser, ToolResults: []ToolResult{
				{ToolUseID: "call_0", Content: "no such file", IsError: true},
			}},
			{Role: RoleAssistant, Text: "the file does not exist"},
		},
		Tools: []Tool{{Name: "read", Description: "read a file", InputSchema: map[string]any{"type": "object"}}},
	}

	got, err := json.Marshal(toChatRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{` +
		`"model":"kimi-k2",` +
		`"messages":[` +
		`{"role":"system","content":"you are a test"},` +
		`{"role":"user","content":"read a.txt"},` +
		`{"role":"assistant","content":"let me read it","tool_calls":[{"id":"call_0","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]},` +
		`{"role":"tool","content":"Error: no such file","tool_call_id":"call_0"},` +
		`{"role":"assistant","content":"the file does not exist"}` +
		`],` +
		`"tools":[{"type":"function","function":{"name":"read","description":"read a file","parameters":{"type":"object"}}}],` +
		`"max_tokens":256,` +
		`"max_completion_tokens":256` +
		`}`
	if string(got) != want {
		t.Fatalf("request json mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestToChatRequestEmptyAssistantText 验证 assistant 文本为空且有 tool_calls 时
// content 被省略（不发 ""），以及空 input 补 {}。
func TestToChatRequestEmptyAssistantText(t *testing.T) {
	req := Request{
		Model:     "m",
		MaxTokens: 1,
		Messages: []Message{
			{Role: RoleAssistant, ToolUses: []ToolUse{{ID: "c1", Name: "list", Input: nil}}},
		},
	}
	got, err := json.Marshal(toChatRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"model":"m","messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"list","arguments":"{}"}}]}` +
		`],"max_tokens":1,"max_completion_tokens":1}`
	if string(got) != want {
		t.Fatalf("request json mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestFromChatResponseGolden 用固定响应 JSON 断言解析出的 IR：
// tool_calls、reasoning_content 进 Thinking、usage、finish_reason 归一化。
func TestFromChatResponseGolden(t *testing.T) {
	raw := `{
		"choices": [{
			"message": {
				"content": "let me check",
				"reasoning_content": "the user wants the file read",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a.txt\"}"}},
					{"id": "call_2", "type": "function", "function": {"name": "list", "arguments": ""}}
				]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 120, "completion_tokens": 34}
	}`
	var wr chatResponse
	if err := json.Unmarshal([]byte(raw), &wr); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	resp, err := fromChatResponse(wr)
	if err != nil {
		t.Fatalf("fromChatResponse: %v", err)
	}

	if resp.Text != "let me check" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
	if resp.Thinking != "the user wants the file read" {
		t.Fatalf("thinking wrong: %q", resp.Thinking)
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.ToolUses) != 2 {
		t.Fatalf("expected 2 tool uses, got %+v", resp.ToolUses)
	}
	if resp.ToolUses[0].ID != "call_1" || resp.ToolUses[0].Name != "read" || string(resp.ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use 0 wrong: %+v", resp.ToolUses[0])
	}
	// 空 arguments 补 {}。
	if string(resp.ToolUses[1].Input) != "{}" {
		t.Fatalf("empty arguments not normalized: %q", resp.ToolUses[1].Input)
	}
}

// TestNormalizeFinishReason 覆盖归一化各分支。
func TestNormalizeFinishReason(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"stop", StopEndTurn},
		{"tool_calls", StopToolUse},
		{"length", StopMaxTokens},
		{"content_filter", StopRefusal},
		{"insufficient_system_resource", StopOther}, // 未知值落 other
		{"", StopOther},
	}
	for _, c := range cases {
		if got := normalizeFinishReason(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestOpenAIComplete 用假服务器端到端验证 Complete：
// 路径、鉴权头、请求编组与响应解析。
func TestOpenAIComplete(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"choices": [{
				"message": {"content": "hello there"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 7, "completion_tokens": 3}
		}`)
	}))
	defer srv.Close()

	c := NewOpenAI("sk-test", srv.URL+"/v1/") // 末尾斜杠要容错

	resp, err := c.Complete(context.Background(), Request{
		Model:     "kimi-k2",
		System:    "you are a test",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path wrong: %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header wrong: %q", gotAuth)
	}
	if gotBody["model"] != "kimi-k2" {
		t.Fatalf("model wrong: %v", gotBody["model"])
	}
	msgs := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are a test" {
		t.Fatalf("system message wrong: %v", first)
	}

	if resp.Text != "hello there" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
	if resp.StopReason != StopEndTurn {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
}

// TestOpenAINoAuthHeader 验证 key 为空时不发 Authorization 头（Ollama 本地无 key）。
func TestOpenAINoAuthHeader(t *testing.T) {
	var gotAuth string
	hasAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, hasAuth = r.Header["Authorization"]
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := NewOpenAI("", srv.URL)
	if _, err := c.Complete(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if hasAuth {
		t.Fatalf("expected no Authorization header, got %q", gotAuth)
	}
}

// TestOpenAIHTTPError 验证非 2xx 返回被包成错误。
func TestOpenAIHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	if _, err := c.Complete(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("expected error on http 401")
	}
}
