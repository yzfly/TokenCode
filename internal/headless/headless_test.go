package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeLLM 按脚本依次返回预设响应；err 非 nil 时直接报错（模拟网络/模型故障）。
type fakeLLM struct {
	responses []llm.Response
	err       error
	calls     int
	lastReq   llm.Request
}

func (f *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.lastReq = req
	if f.err != nil {
		return llm.Response{}, f.err
	}
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

// stubTool 记录是否被执行，返回固定结果。
type stubTool struct {
	name string
	ran  *bool
}

func (s stubTool) Name() string           { return s.name }
func (s stubTool) Description() string    { return s.name }
func (s stubTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if s.ran != nil {
		*s.ran = true
	}
	return "stub ok", nil
}

// newAgent 装配一个带白名单守卫的测试 agent（与 cmd 装配同构）。
func newAgent(fake *fakeLLM, allowed []string, yolo bool, ts ...tools.Tool) *agent.Agent {
	allow := Allow(allowed, yolo)
	reg := tools.NewRegistry()
	for _, t := range ts {
		reg.Add(GateTool(t, allow))
	}
	return agent.New(fake, reg, "test-model", 1024)
}

func TestRunCollectsResultAndEvents(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{{ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		{Text: "Done.", StopReason: "end_turn"},
	}}
	ag := newAgent(fake, []string{"echo"}, false, stubTool{name: "echo"})

	var events []Event
	res := Run(context.Background(), ag, "test-model", "go", func(ev Event) { events = append(events, ev) })

	if res.IsError || res.Result != "Done." || res.ToolCalls != 1 || res.Model != "test-model" {
		t.Fatalf("result wrong: %+v", res)
	}
	// 事件顺序：tool_call → tool_result → assistant_delta → result（最后恒为 result）。
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	want := []string{"tool_call", "tool_result", "assistant_delta", "result"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event order = %v, want %v", types, want)
	}
	last := events[len(events)-1]
	if last.Result != "Done." || last.ToolCalls == nil || *last.ToolCalls != 1 || last.IsError {
		t.Fatalf("result event wrong: %+v", last)
	}
}

func TestGateRejectsOutsideWhitelist(t *testing.T) {
	ran := false
	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{{ID: "c1", Name: "danger", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		{Text: "ok, skipped", StopReason: "end_turn"},
	}}
	ag := newAgent(fake, []string{"read"}, false, stubTool{name: "danger", ran: &ran})

	var events []Event
	res := Run(context.Background(), ag, "m", "go", func(ev Event) { events = append(events, ev) })
	if ran {
		t.Fatal("whitelisted-out tool must not execute")
	}
	if res.IsError {
		t.Fatalf("turn should finish normally: %+v", res)
	}
	// 拒绝以错误 tool_result 喂回模型，内容含 "rejected (headless)"。
	var fed llm.ToolResult
	for _, m := range fake.lastReq.Messages {
		for _, tr := range m.ToolResults {
			fed = tr
		}
	}
	if !fed.IsError || !strings.Contains(fed.Content, "rejected (headless)") {
		t.Fatalf("fed-back tool_result wrong: %+v", fed)
	}
	for _, ev := range events {
		if ev.Type == "tool_result" && !ev.IsError {
			t.Fatalf("tool_result event should be error: %+v", ev)
		}
	}
}

func TestGateYoloAllowsAll(t *testing.T) {
	ran := false
	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{{ID: "c1", Name: "danger", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	}}
	ag := newAgent(fake, nil, true, stubTool{name: "danger", ran: &ran})
	if res := Run(context.Background(), ag, "m", "go", nil); res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	if !ran {
		t.Fatal("yolo should allow the tool to run")
	}
}

type concStub struct{ stubTool }

func (concStub) Concurrent() bool { return true }

func TestGateToolKeepsConcurrentMark(t *testing.T) {
	g := GateTool(concStub{stubTool{name: "x"}}, Allow(nil, true))
	c, ok := g.(tools.Concurrent)
	if !ok || !c.Concurrent() {
		t.Fatal("gate must pass through the Concurrent mark")
	}
	g = GateTool(stubTool{name: "y"}, Allow(nil, true))
	if c, ok := g.(tools.Concurrent); ok && c.Concurrent() {
		t.Fatal("plain tool must not become concurrent")
	}
}

func TestExecuteText(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "hello world", StopReason: "end_turn"}}}
	ag := newAgent(fake, nil, false)
	var buf bytes.Buffer
	res := Execute(context.Background(), ag, "m", "hi", "text", &buf)
	if res.IsError || buf.String() != "hello world\n" {
		t.Fatalf("text output = %q, res = %+v", buf.String(), res)
	}
}

func TestExecuteJSON(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "hi there", StopReason: "end_turn"}}}
	ag := newAgent(fake, nil, false)
	var buf bytes.Buffer
	Execute(context.Background(), ag, "test-model", "hi", "json", &buf)

	var res Result
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("not a json object: %v\n%s", err, buf.String())
	}
	if res.Result != "hi there" || res.Model != "test-model" || res.IsError || res.ToolCalls != 0 {
		t.Fatalf("json result wrong: %+v", res)
	}
}

func TestExecuteStreamJSONLastLineIsResult(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{{ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		{Text: "Done.", StopReason: "end_turn"},
	}}
	ag := newAgent(fake, []string{"echo"}, false, stubTool{name: "echo"})
	var buf bytes.Buffer
	Execute(context.Background(), ag, "m", "go", "stream-json", &buf)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected multiple JSONL lines, got %q", buf.String())
	}
	var last Event
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d not json: %v: %q", i, err, line)
		}
		if ev.Type == "result" && i != len(lines)-1 {
			t.Fatalf("result event must be the last line, found at %d/%d", i, len(lines))
		}
		last = ev
	}
	if last.Type != "result" || last.Result != "Done." || last.ToolCalls == nil || *last.ToolCalls != 1 {
		t.Fatalf("last line wrong: %+v", last)
	}
}

func TestRunErrorBecomesIsError(t *testing.T) {
	fake := &fakeLLM{err: errors.New("connection refused")}
	ag := newAgent(fake, nil, false)
	var buf bytes.Buffer
	res := Execute(context.Background(), ag, "m", "hi", "stream-json", &buf)
	if !res.IsError || !strings.Contains(res.Result, "connection refused") {
		t.Fatalf("error not surfaced: %+v", res)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var last Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Type != "result" || !last.IsError || !strings.Contains(last.Result, "connection refused") {
		t.Fatalf("error result event wrong: %+v", last)
	}
}
