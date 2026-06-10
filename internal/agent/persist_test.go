package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeStreamer 在 fakeLLM 之上实现 Streamer：把 Text 按字符流出去。
type fakeStreamer struct{ fakeLLM }

func (f *fakeStreamer) CompleteStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	resp, err := f.Complete(ctx, req)
	if err == nil && onDelta != nil {
		for _, r := range resp.Text {
			onDelta(llm.Delta{Text: string(r)})
		}
	}
	return resp, err
}

// TestRunStreamsWhenSupported 验证：codec 支持流式且外壳给了增量回调时
// 走流式，增量拼起来等于最终文本，OnAssistant 仍收到完整文本。
func TestRunStreamsWhenSupported(t *testing.T) {
	fake := &fakeStreamer{fakeLLM{responses: []llm.Response{{Text: "hello", StopReason: llm.StopEndTurn}}}}
	a := New(fake, tools.NewRegistry(), "m", 64)

	var deltas strings.Builder
	var final string
	err := a.Run(context.Background(), "hi", UI{
		OnAssistantDelta: func(d string) { deltas.WriteString(d) },
		OnAssistant:      func(s string) { final = s },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if deltas.String() != "hello" || final != "hello" {
		t.Fatalf("deltas %q / final %q", deltas.String(), final)
	}
}

// TestPersistWatermark 验证：每拍结束后只把新增消息交给 persist，多拍不重复。
func TestPersistWatermark(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "one", StopReason: llm.StopEndTurn},
		{Text: "two", StopReason: llm.StopEndTurn},
	}}
	a := New(fake, tools.NewRegistry(), "m", 64)

	var batches [][]llm.Message
	a.SetPersist(func(ms []llm.Message) { batches = append(batches, ms) })

	if err := a.Run(context.Background(), "first", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := a.Run(context.Background(), "second", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(batches) != 2 {
		t.Fatalf("expected 2 batches, got %d", len(batches))
	}
	if len(batches[0]) != 2 || batches[0][0].Text != "first" || batches[0][1].Text != "one" {
		t.Fatalf("batch 0 wrong: %+v", batches[0])
	}
	if len(batches[1]) != 2 || batches[1][0].Text != "second" || batches[1][1].Text != "two" {
		t.Fatalf("batch 1 wrong: %+v", batches[1])
	}
}

// TestPersistSkipsDroppedIdleTurn 验证：心跳空转拍被剔除后不落盘。
func TestPersistSkipsDroppedIdleTurn(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: Sentinel, StopReason: llm.StopEndTurn},
		{Text: "real work", StopReason: llm.StopEndTurn},
	}}
	a := New(fake, tools.NewRegistry(), "m", 64)

	var batches [][]llm.Message
	a.SetPersist(func(ms []llm.Message) { batches = append(batches, ms) })

	events := make(chan Event, 2)
	events <- Event{Source: SourceHeartbeat, Text: "heartbeat check", Ephemeral: true}
	events <- Event{Source: SourceUser, Text: "do something"}
	close(events)
	a.Serve(context.Background(), events, UI{})

	if len(batches) != 1 {
		t.Fatalf("expected 1 batch (idle turn dropped), got %d: %+v", len(batches), batches)
	}
	if batches[0][0].Text != "do something" || batches[0][1].Text != "real work" {
		t.Fatalf("batch wrong: %+v", batches[0])
	}
}

// TestSeedSetsWatermark 验证：Seed 注入的历史视为已持久化，不会重复落盘。
func TestSeedSetsWatermark(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "resumed reply", StopReason: llm.StopEndTurn}}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	a.Seed([]llm.Message{
		{Role: llm.RoleUser, Text: "old"},
		{Role: llm.RoleAssistant, Text: "history"},
	})

	var batches [][]llm.Message
	a.SetPersist(func(ms []llm.Message) { batches = append(batches, ms) })

	if err := a.Run(context.Background(), "new input", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(batches) != 1 || len(batches[0]) != 2 || batches[0][0].Text != "new input" {
		t.Fatalf("expected only fresh messages persisted, got %+v", batches)
	}
	// 注入的历史确实参与请求上下文。
	if len(fake.lastReq.Messages) != 3 {
		t.Fatalf("expected seeded history in request, got %d messages", len(fake.lastReq.Messages))
	}
}
