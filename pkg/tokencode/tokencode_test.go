package tokencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLLM 按脚本依次吐 Response，并记录收到的请求（多轮历史断言用）。
type fakeLLM struct {
	script []Response
	reqs   []Request
}

func (f *fakeLLM) Complete(_ context.Context, req Request) (Response, error) {
	f.reqs = append(f.reqs, req)
	if len(f.script) == 0 {
		return Response{Text: "(script exhausted)"}, nil
	}
	resp := f.script[0]
	f.script = f.script[1:]
	return resp, nil
}

// isolateEnv 把 config/usage 的落盘目录指到临时目录，测试不碰真实用户数据。
func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// echoTool 是测试用自定义工具：原样回显 input 里的 msg。
type echoTool struct{ calls *int }

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo back msg" }
func (echoTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"msg": map[string]any{"type": "string"},
	}}
}
func (e echoTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	if e.calls != nil {
		*e.calls++
	}
	var in struct{ Msg string }
	_ = json.Unmarshal(input, &in)
	return "echo: " + in.Msg, nil
}

func TestRunMultiTurnHistory(t *testing.T) {
	isolateEnv(t)
	fake := &fakeLLM{script: []Response{{Text: "回答一"}, {Text: "回答二"}}}
	tc, err := New(WithLLM(fake, "fake-model"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if tc.Model() != "fake-model" {
		t.Fatalf("Model() = %q, want fake-model", tc.Model())
	}

	out, err := tc.Run(context.Background(), "第一问")
	if err != nil || out != "回答一" {
		t.Fatalf("Run #1 = (%q, %v), want 回答一", out, err)
	}
	out, err = tc.Run(context.Background(), "第二问")
	if err != nil || out != "回答二" {
		t.Fatalf("Run #2 = (%q, %v), want 回答二", out, err)
	}

	// 第二次请求必须带上第一轮历史：user/assistant/user 共 3 条。
	if len(fake.reqs) != 2 || len(fake.reqs[1].Messages) != 3 {
		t.Fatalf("第二次请求历史条数 = %d, want 3", len(fake.reqs[1].Messages))
	}
	h := tc.History()
	if len(h) != 4 || h[0].Role != RoleUser || h[3].Text != "回答二" {
		t.Fatalf("History 形状不对: %+v", h)
	}

	tc.Reset()
	if len(tc.History()) != 0 {
		t.Fatal("Reset 后历史应为空")
	}
}

func TestAddToolLoop(t *testing.T) {
	isolateEnv(t)
	calls := 0
	fake := &fakeLLM{script: []Response{
		{ToolUses: []ToolUse{{ID: "t1", Name: "echo", Input: json.RawMessage(`{"msg":"hi"}`)}}},
		{Text: "done"},
	}}
	tc, err := New(WithLLM(fake, "fake-model"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc.AddTool(echoTool{calls: &calls})

	out, err := tc.Run(context.Background(), "调一下 echo")
	if err != nil || out != "done" {
		t.Fatalf("Run = (%q, %v), want done", out, err)
	}
	if calls != 1 {
		t.Fatalf("echo 执行次数 = %d, want 1", calls)
	}
	// 第二次模型调用要能看到工具结果回灌。
	last := fake.reqs[1].Messages[len(fake.reqs[1].Messages)-1]
	if len(last.ToolResults) != 1 || last.ToolResults[0].Content != "echo: hi" {
		t.Fatalf("工具结果未回灌: %+v", last)
	}
	// 工具定义也要随请求下发。
	if len(fake.reqs[0].Tools) != 1 || fake.reqs[0].Tools[0].Name != "echo" {
		t.Fatalf("请求工具定义不对: %+v", fake.reqs[0].Tools)
	}
}

func TestRunStreamEventOrder(t *testing.T) {
	isolateEnv(t)
	fake := &fakeLLM{script: []Response{
		{ToolUses: []ToolUse{{ID: "t1", Name: "echo", Input: json.RawMessage(`{"msg":"x"}`)}}},
		{Text: "final"},
	}}
	tc, err := New(WithLLM(fake, "fake-model"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc.AddTool(echoTool{})

	var types []string
	var final Event
	err = tc.RunStream(context.Background(), "go", func(ev Event) {
		types = append(types, ev.Type)
		final = ev
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	want := []string{"tool_call", "tool_result", "assistant_delta", "result"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("事件顺序 = %v, want %v", types, want)
	}
	if final.Type != "result" || final.Result != "final" || final.ToolCalls == nil || *final.ToolCalls != 1 {
		t.Fatalf("result 事件不对: %+v", final)
	}

	if err := tc.RunStream(context.Background(), "x", nil); err == nil {
		t.Fatal("nil 回调应当报错")
	}
}

func TestWithRootIsolation(t *testing.T) {
	isolateEnv(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLLM{script: []Response{
		{ToolUses: []ToolUse{
			{ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"in.txt"}`)},
			{ID: "t2", Name: "read", Input: json.RawMessage(`{"path":` + jsonQuote(outside) + `}`)},
		}},
		{Text: "done"},
	}}
	tc, err := New(WithLLM(fake, "fake-model"), WithTools(DefaultTools()), WithRoot(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := tc.Run(context.Background(), "读两个文件"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	results := fake.reqs[1].Messages[len(fake.reqs[1].Messages)-1].ToolResults
	if len(results) != 2 {
		t.Fatalf("工具结果数 = %d, want 2", len(results))
	}
	// 根内相对路径可读；根外绝对路径必须被隔离拒绝。
	if results[0].IsError || !strings.Contains(results[0].Content, "inside") {
		t.Fatalf("根内读取失败: %+v", results[0])
	}
	if !results[1].IsError || !strings.Contains(results[1].Content, "隔离") {
		t.Fatalf("根外读取未被拦截: %+v", results[1])
	}
}

func TestAllowedToolsGate(t *testing.T) {
	isolateEnv(t)
	fake := &fakeLLM{script: []Response{
		{ToolUses: []ToolUse{{ID: "t1", Name: "echo", Input: json.RawMessage(`{"msg":"x"}`)}}},
		{Text: "done"},
	}}
	calls := 0
	tc, err := New(WithLLM(fake, "fake-model"), WithAllowedTools("read"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc.AddTool(echoTool{calls: &calls}) // 不在白名单：AddTool 之后同样受守卫

	if _, err := tc.Run(context.Background(), "试试越权"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 0 {
		t.Fatal("白名单外的工具不应被执行")
	}
	res := fake.reqs[1].Messages[len(fake.reqs[1].Messages)-1].ToolResults[0]
	if !res.IsError || !strings.Contains(res.Content, "rejected") {
		t.Fatalf("越权调用应以错误回灌: %+v", res)
	}
}

func TestNewMissingCredentials(t *testing.T) {
	isolateEnv(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("缺凭据应报带指引的错误, got: %v", err)
	}
}

// jsonQuote 把字符串编码成 JSON 字面量（拼 input 用）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
