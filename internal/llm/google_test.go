package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestToGeminiRequestGolden 用固定 IR 输入断言生成的 JSON：
// 覆盖 system_instruction、tool 循环往返一整轮（user → model 带 functionCall →
// functionResponse（按 ID→name 映射还原）→ model 文本）、IsError 走 error 键、
// generationConfig、functionDeclarations。
func TestToGeminiRequestGolden(t *testing.T) {
	req := Request{
		Model:     "gemini-2.5-pro",
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

	got, err := json.Marshal(toGeminiRequest(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{` +
		`"system_instruction":{"parts":[{"text":"you are a test"}]},` +
		`"contents":[` +
		`{"role":"user","parts":[{"text":"read a.txt"}]},` +
		`{"role":"model","parts":[{"text":"let me read it"},{"functionCall":{"name":"read","args":{"path":"a.txt"}}}]},` +
		`{"role":"user","parts":[{"functionResponse":{"name":"read","response":{"error":"no such file"}}}]},` +
		`{"role":"model","parts":[{"text":"the file does not exist"}]}` +
		`],` +
		`"tools":[{"functionDeclarations":[{"name":"read","description":"read a file","parameters":{"type":"object"}}]}],` +
		`"generationConfig":{"maxOutputTokens":256}` +
		`}`
	if string(got) != want {
		t.Fatalf("request json mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestFromGeminiResponseGolden 用固定响应 JSON 断言解析出的 IR：
// thought part 进 Thinking、functionCall 合成 ID、有 functionCall 即 StopToolUse、usage。
func TestFromGeminiResponseGolden(t *testing.T) {
	raw := `{
		"candidates": [{
			"content": {"role": "model", "parts": [
				{"text": "the user wants the file read", "thought": true},
				{"text": "let me check"},
				{"functionCall": {"name": "read", "args": {"path":"a.txt"}}},
				{"functionCall": {"name": "list"}}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 120, "candidatesTokenCount": 34}
	}`
	var wr geminiResponse
	if err := json.Unmarshal([]byte(raw), &wr); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	resp, err := fromGeminiResponse(wr)
	if err != nil {
		t.Fatalf("fromGeminiResponse: %v", err)
	}

	if resp.Text != "let me check" {
		t.Fatalf("text wrong: %q", resp.Text)
	}
	if resp.Thinking != "the user wants the file read" {
		t.Fatalf("thinking wrong: %q", resp.Thinking)
	}
	// finishReason 是 STOP，但有 functionCall 即继续调工具。
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop_reason wrong: %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 34 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.ToolUses) != 2 {
		t.Fatalf("expected 2 tool uses, got %+v", resp.ToolUses)
	}
	if resp.ToolUses[0].ID != "read-2" || resp.ToolUses[0].Name != "read" || string(resp.ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use 0 wrong: %+v", resp.ToolUses[0])
	}
	// 无 args 补 {}。
	if string(resp.ToolUses[1].Input) != "{}" {
		t.Fatalf("empty args not normalized: %q", resp.ToolUses[1].Input)
	}
}

// TestFromGeminiResponseError 验证响应体内嵌 error 对象被包成错误。
func TestFromGeminiResponseError(t *testing.T) {
	var wr geminiResponse
	if err := json.Unmarshal([]byte(`{"error":{"code":400,"message":"bad request"}}`), &wr); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if _, err := fromGeminiResponse(wr); err == nil {
		t.Fatal("expected error from embedded error object")
	}
}

// TestNormalizeGeminiFinish 覆盖归一化各分支。
func TestNormalizeGeminiFinish(t *testing.T) {
	cases := []struct {
		raw  string
		want StopReason
	}{
		{"STOP", StopEndTurn},
		{"MAX_TOKENS", StopMaxTokens},
		{"SAFETY", StopRefusal},
		{"RECITATION", StopRefusal},
		{"MALFORMED_FUNCTION_CALL", StopOther}, // 未知值落 other
		{"", StopOther},
	}
	for _, c := range cases {
		if got := normalizeGeminiFinish(c.raw); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestGoogleComplete 用假服务器端到端验证 Complete：
// 路径（模型名内嵌）、x-goog-api-key 头、请求编组与响应解析。
func TestGoogleComplete(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": "hello there"}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3}
		}`)
	}))
	defer srv.Close()

	c := NewGoogle("g-test", srv.URL+"/") // 末尾斜杠要容错

	resp, err := c.Complete(context.Background(), Request{
		Model:     "gemini-2.5-flash",
		System:    "you are a test",
		MaxTokens: 256,
		Messages:  []Message{{Role: RoleUser, Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("path wrong: %q", gotPath)
	}
	if gotKey != "g-test" {
		t.Fatalf("api key header wrong: %q", gotKey)
	}
	si := gotBody["system_instruction"].(map[string]any)
	parts := si["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "you are a test" {
		t.Fatalf("system_instruction wrong: %v", si)
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

// TestGoogleHTTPError 验证非 2xx 返回被包成错误。
func TestGoogleHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":403,"message":"key invalid"}}`)
	}))
	defer srv.Close()

	c := NewGoogle("x", srv.URL)
	if _, err := c.Complete(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []Message{{Role: RoleUser, Text: "hi"}}}); err == nil {
		t.Fatal("expected error on http 403")
	}
}
