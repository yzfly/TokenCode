package agent

import (
	"context"
	"testing"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// serveHarness 起一个 Serve actor，返回事件入口与逐拍完成信号。
func serveHarness(t *testing.T, fake *fakeLLM) (*Agent, chan<- Event, <-chan error, *[]string) {
	t.Helper()
	a := New(fake, tools.NewRegistry(), "m", 100)
	events := make(chan Event)
	done := make(chan error, 8)
	var shown []string
	ui := UI{
		OnAssistant: func(text string) { shown = append(shown, text) },
		OnTurnDone:  func(_ EventSource, err error) { done <- err },
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Serve(ctx, events, ui)
	return a, events, done, &shown
}

// 心跳空转：assistant 回哨兵 → 这一拍的 user+assistant 从历史剔除，且哨兵不上屏。
func TestServeEphemeralSentinelDropped(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: Sentinel, StopReason: "end_turn"},
		{Text: "回答", StopReason: "end_turn"},
	}}
	a, events, done, shown := serveHarness(t, fake)

	events <- Event{Source: SourceHeartbeat, Text: "<heartbeat/>", Ephemeral: true}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := a.HistoryLen(); n != 0 {
		t.Fatalf("空转后历史应为 0 条，得到 %d", n)
	}
	if len(*shown) != 0 {
		t.Fatalf("哨兵不应上屏，得到 %v", *shown)
	}

	// 普通用户事件照常入史。
	events <- Event{Source: SourceUser, Text: "hi"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := a.HistoryLen(); n != 2 {
		t.Fatalf("用户 turn 后历史应为 2 条，得到 %d", n)
	}
	if len(*shown) != 1 || (*shown)[0] != "回答" {
		t.Fatalf("用户 turn 的回复应上屏，得到 %v", *shown)
	}
}

// 心跳真有事：assistant 回非哨兵文本 → 这一拍保留在历史里、正常上屏。
func TestServeEphemeralRealReportKept(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "工作区有改动，建议提交", StopReason: "end_turn"},
	}}
	a, events, done, shown := serveHarness(t, fake)

	events <- Event{Source: SourceHeartbeat, Text: "<heartbeat/>", Ephemeral: true}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := a.HistoryLen(); n != 2 {
		t.Fatalf("有实质报告的拍应保留，历史应为 2 条，得到 %d", n)
	}
	if len(*shown) != 1 {
		t.Fatalf("实质报告应上屏，得到 %v", *shown)
	}
}

// OnTurnStart 携带来源与可用的 cancel；Snapshot 是拷贝、不随后续追加变化。
func TestServeTurnStartAndSnapshot(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "ok", StopReason: "end_turn"},
	}}
	a := New(fake, tools.NewRegistry(), "m", 100)
	events := make(chan Event)
	done := make(chan error, 1)
	var src EventSource
	ui := UI{
		OnTurnStart: func(s EventSource, cancel context.CancelFunc) {
			src = s
			if cancel == nil {
				t.Error("cancel 不应为 nil")
			}
		},
		OnTurnDone: func(_ EventSource, err error) { done <- err },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Serve(ctx, events, ui)

	events <- Event{Source: SourceUser, Text: "hello"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if src != SourceUser {
		t.Fatalf("来源应为 user，得到 %q", src)
	}

	snap := a.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("快照应有 2 条，得到 %d", len(snap))
	}
	a.append(llm.Message{Role: llm.RoleUser, Text: "later"})
	if len(snap) != 2 {
		t.Fatal("快照不应随后续追加变化")
	}
}
