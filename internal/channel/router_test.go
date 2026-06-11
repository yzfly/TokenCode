package channel

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeAdapter 记录所有出站消息，Start 不用（测试直接调 Handle）。
type fakeAdapter struct {
	mu    sync.Mutex
	sends []Outbound
}

func (f *fakeAdapter) Name() string                                        { return "fake" }
func (f *fakeAdapter) Start(ctx context.Context, sink func(Inbound)) error { <-ctx.Done(); return nil }
func (f *fakeAdapter) Send(_ context.Context, out Outbound) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, out)
	return nil
}

func (f *fakeAdapter) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sends))
	for i, s := range f.sends {
		out[i] = s.Text
	}
	return out
}

// fakeLLM 每次调用都返回同一段文本（无工具调用，一拍即收）。
type fakeLLM struct {
	mu    sync.Mutex
	text  string
	block chan struct{} // 非 nil 时 Complete 阻塞至其关闭（测「稍候」用）
	reqs  []llm.Request
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return llm.Response{Text: f.text, StopReason: "end_turn"}, nil
}

// newTestRouter 装配一套「fake adapter + fake LLM」的路由。
func newTestRouter(t *testing.T, fake *fakeLLM) (*Router, *fakeAdapter, *Store) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "team.json"))
	var assembled []Binding
	r := NewRouter(store, func(b Binding) (*agent.Agent, string, error) {
		assembled = append(assembled, b)
		ag := agent.New(fake, tools.NewRegistry(), "test-model", 256)
		ag.SetUsageSource("channel:" + b.Channel)
		return ag, "test-model", nil
	}, t.Logf)
	ad := &fakeAdapter{}
	r.Register(ad)
	return r, ad, store
}

func in(user, chat, text string) Inbound {
	return Inbound{Channel: "fake", UserID: user, ChatID: chat, Text: text}
}

func TestUnboundUserGetsPairHint(t *testing.T) {
	r, ad, _ := newTestRouter(t, &fakeLLM{text: "hi"})
	r.Handle(context.Background(), in("u1", "c1", "帮我修个 bug"))
	got := ad.texts()
	if len(got) != 1 || !strings.Contains(got[0], "配对码") {
		t.Fatalf("未绑定用户应收到配对引导: %v", got)
	}
}

func TestPairingViaCode(t *testing.T) {
	r, ad, store := newTestRouter(t, &fakeLLM{text: "hi"})
	_ = store.AddPending(Pending{Code: "ABCD2345", Workspace: "/ws/a", ExpiresAt: time.Now().Add(PairTTL)})

	r.Handle(context.Background(), in("u1", "c1", "abcd2345"))
	got := ad.texts()
	if len(got) != 1 || !strings.Contains(got[0], "配对成功") {
		t.Fatalf("发码应配对成功: %v", got)
	}
	if _, ok, _ := store.Find("fake", "u1"); !ok {
		t.Fatal("绑定没落盘")
	}

	// 错码（像码但不存在）→ 无效提示。
	r.Handle(context.Background(), in("u2", "c2", "ZZZZ9999"))
	got = ad.texts()
	if !strings.Contains(got[len(got)-1], "无效或已过期") {
		t.Fatalf("错码应提示无效: %v", got)
	}
}

func TestBoundUserRunsTurn(t *testing.T) {
	fake := &fakeLLM{text: "改好了，跑了 3 个测试全绿。"}
	r, ad, store := newTestRouter(t, fake)
	_ = store.AddPending(Pending{Code: "ABCD2345", Workspace: "/ws/a", ExpiresAt: time.Now().Add(PairTTL)})
	_, _, _ = store.Pair("fake", "u1", "", "ABCD2345")

	r.Handle(context.Background(), in("u1", "c1", "修一下 bug"))
	got := ad.texts()
	if len(got) != 2 {
		t.Fatalf("应有「收到」+最终文本两条回复: %v", got)
	}
	if !strings.Contains(got[0], "收到") {
		t.Fatalf("第一条应是进度回报: %q", got[0])
	}
	if got[1] != fake.text {
		t.Fatalf("第二条应是最终文本: %q", got[1])
	}
	// 会话连续：第二条消息复用同一 agent（历史累积，请求消息数变多）。
	r.Handle(context.Background(), in("u1", "c1", "再检查一遍"))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.reqs) != 2 {
		t.Fatalf("应有两次模型调用，got %d", len(fake.reqs))
	}
	if len(fake.reqs[1].Messages) <= len(fake.reqs[0].Messages) {
		t.Fatal("第二拍应携带第一拍的历史（常驻会话）")
	}
}

func TestBusySessionSaysWait(t *testing.T) {
	fake := &fakeLLM{text: "done", block: make(chan struct{})}
	r, ad, store := newTestRouter(t, fake)
	_ = store.AddPending(Pending{Code: "ABCD2345", Workspace: "/ws/a", ExpiresAt: time.Now().Add(PairTTL)})
	_, _, _ = store.Pair("fake", "u1", "", "ABCD2345")

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Handle(context.Background(), in("u1", "c1", "慢任务"))
	}()
	// 等第一条「收到」发出（即 turn 已持锁在跑）。
	deadline := time.After(2 * time.Second)
	for {
		if len(ad.texts()) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("等不到进度回报")
		case <-time.After(5 * time.Millisecond):
		}
	}

	r.Handle(context.Background(), in("u1", "c1", "插队消息"))
	got := ad.texts()
	if !strings.Contains(got[len(got)-1], "稍候") {
		t.Fatalf("在跑时应提示稍候: %v", got)
	}

	close(fake.block)
	<-done
	final := ad.texts()
	if final[len(final)-1] != "done" {
		t.Fatalf("慢任务最终文本应送达: %v", final)
	}
}

func TestEmptyTextIgnored(t *testing.T) {
	r, ad, _ := newTestRouter(t, &fakeLLM{text: "hi"})
	r.Handle(context.Background(), in("u1", "c1", "   "))
	if len(ad.texts()) != 0 {
		t.Fatalf("空白消息应被忽略: %v", ad.texts())
	}
}

func TestUnknownChannelDropped(t *testing.T) {
	r, _, _ := newTestRouter(t, &fakeLLM{text: "hi"})
	// 不注册的通道：不 panic、不回复。
	r.Handle(context.Background(), Inbound{Channel: "ghost", UserID: "u", ChatID: "c", Text: "x"})
}
