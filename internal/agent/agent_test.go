package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
