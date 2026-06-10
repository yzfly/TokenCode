package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeLLM 按脚本依次返回预设响应，并记录最后一次请求。
type fakeLLM struct {
	responses []llm.Response
	calls     int
	lastReq   llm.Request
}

func (f *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.lastReq = req
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

func TestAgentToolLoop(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hello.txt")
	writeArgs, _ := json.Marshal(map[string]any{"path": p, "content": "hi\n"})

	fake := &fakeLLM{responses: []llm.Response{
		// 第一轮：要求调用 write
		{ToolUses: []llm.ToolUse{{ID: "call_1", Name: "write", Input: writeArgs}}, StopReason: "tool_use"},
		// 第二轮：收到 tool_result 后给最终答复
		{Text: "Done.", StopReason: "end_turn"},
	}}

	a := New(fake, tools.NewRegistry(tools.Read(), tools.Write(), tools.Edit(), tools.Bash()), "test-model", 1024)

	var finalText, toolRan string
	err := a.Run(context.Background(), "create hello.txt with hi", UI{
		OnAssistant: func(s string) { finalText = s },
		OnToolCall:  func(name string, in json.RawMessage) bool { toolRan = name; return true },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("expected 2 llm calls, got %d", fake.calls)
	}
	if toolRan != "write" {
		t.Fatalf("expected write tool, got %q", toolRan)
	}
	if finalText != "Done." {
		t.Fatalf("expected final text 'Done.', got %q", finalText)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "hi\n" {
		t.Fatalf("file content wrong: %q err=%v", b, err)
	}

	// tool_result 必须被回灌：最后一次请求里应有带该 tool_use_id 的 user 消息。
	found := false
	for _, m := range fake.lastReq.Messages {
		for _, tr := range m.ToolResults {
			if tr.ToolUseID == "call_1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tool result was not fed back to the model")
	}
}

func TestAgentRejection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	writeArgs, _ := json.Marshal(map[string]any{"path": p, "content": "nope"})

	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{{ID: "c1", Name: "write", Input: writeArgs}}, StopReason: "tool_use"},
		{Text: "ok, skipped", StopReason: "end_turn"},
	}}

	a := New(fake, tools.NewRegistry(tools.Write()), "m", 100)
	err := a.Run(context.Background(), "write x", UI{
		OnToolCall: func(name string, in json.RawMessage) bool { return false }, // 拒绝
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("file should not exist after rejection")
	}
	// 拒绝信息仍作为 tool_result（标记为错误）回灌。
	var gotResult llm.ToolResult
	for _, m := range fake.lastReq.Messages {
		for _, tr := range m.ToolResults {
			gotResult = tr
		}
	}
	if gotResult.ToolUseID != "c1" || !gotResult.IsError {
		t.Fatalf("expected rejected tool_result for c1, got %+v", gotResult)
	}
}

// waitSignalTool 用一对会合工具证明并行执行：wait 阻塞等 signal 发令。
// 串行执行时 wait 先跑会超时报错；只有并发时两者都能立即完成。
type waitSignalTool struct {
	name string
	ch   chan struct{}
}

func (w waitSignalTool) Name() string           { return w.name }
func (w waitSignalTool) Description() string    { return w.name }
func (w waitSignalTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (w waitSignalTool) Concurrent() bool       { return true }
func (w waitSignalTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if w.name == "signal" {
		close(w.ch)
		return "signaled", nil
	}
	select {
	case <-w.ch:
		return "got signal", nil
	case <-time.After(5 * time.Second):
		return "", fmt.Errorf("timeout: tools ran sequentially")
	}
}

func TestConcurrentToolsRunInParallel(t *testing.T) {
	ch := make(chan struct{})
	fake := &fakeLLM{responses: []llm.Response{
		{ToolUses: []llm.ToolUse{
			{ID: "w1", Name: "wait", Input: json.RawMessage(`{}`)},
			{ID: "s1", Name: "signal", Input: json.RawMessage(`{}`)},
		}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	}}
	reg := tools.NewRegistry(waitSignalTool{"wait", ch}, waitSignalTool{"signal", ch})
	a := New(fake, reg, "m", 100)

	if err := a.Run(context.Background(), "go", UI{}); err != nil {
		t.Fatal(err)
	}
	// 两个结果都成功且按原顺序回灌。
	var trs []llm.ToolResult
	for _, m := range fake.lastReq.Messages {
		if len(m.ToolResults) > 0 {
			trs = m.ToolResults
		}
	}
	if len(trs) != 2 || trs[0].ToolUseID != "w1" || trs[1].ToolUseID != "s1" {
		t.Fatalf("results wrong: %+v", trs)
	}
	for _, tr := range trs {
		if tr.IsError {
			t.Fatalf("tool errored (sequential execution?): %+v", tr)
		}
	}
}

func TestMaxCallsGuard(t *testing.T) {
	// 模型永远要求调工具：maxCalls 应当掐断循环。
	loop := llm.Response{ToolUses: []llm.ToolUse{{ID: "x", Name: "noop", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"}
	fake := &fakeLLM{responses: []llm.Response{loop, loop, loop, loop, loop}}
	a := New(fake, tools.NewRegistry(noopTool{}), "m", 100)
	a.SetMaxCalls(3)
	err := a.Run(context.Background(), "go", UI{})
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("want max-calls error, got %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("expected exactly 3 llm calls, got %d", fake.calls)
	}
}

type noopTool struct{}

func (noopTool) Name() string           { return "noop" }
func (noopTool) Description() string    { return "noop" }
func (noopTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (noopTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

func TestSetSystemOverride(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "hi", StopReason: "end_turn"}}}
	a := New(fake, tools.NewRegistry(), "m", 100)
	a.SetSystem("custom sub-agent prompt")
	if err := a.Run(context.Background(), "x", UI{}); err != nil {
		t.Fatal(err)
	}
	if fake.lastReq.System != "custom sub-agent prompt" {
		t.Fatalf("system = %q", fake.lastReq.System)
	}
}
