package pulse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
)

// idleSince 造一个最近活动在 d 之前的追踪器。
func idleSince(d time.Duration) *IdleTracker {
	t := &IdleTracker{}
	t.last.Store(time.Now().Add(-d).UnixNano())
	return t
}

func TestIdleTracker(t *testing.T) {
	tr := NewIdleTracker()
	if got := tr.IdleFor(); got > time.Second {
		t.Fatalf("刚创建即视为活跃，IdleFor 应接近 0，得到 %v", got)
	}
	tr.last.Store(time.Now().Add(-time.Hour).UnixNano())
	if got := tr.IdleFor(); got < 59*time.Minute {
		t.Fatalf("IdleFor 应约 1h，得到 %v", got)
	}
	tr.Touch()
	if got := tr.IdleFor(); got > time.Second {
		t.Fatalf("Touch 后 IdleFor 应归零，得到 %v", got)
	}
}

func TestBeatL0AllEmptySkips(t *testing.T) {
	events := make(chan agent.Event, 1)
	p := New(Config{HeartbeatInterval: time.Minute}, events, idleSince(time.Hour), nil,
		func() string { return "" },
		func() string { return "" },
	)
	p.beat(context.Background())
	select {
	case ev := <-events:
		t.Fatalf("L0 全空不应投事件，得到 %+v", ev)
	default:
	}
}

func TestBeatReportPostsEphemeralHeartbeat(t *testing.T) {
	events := make(chan agent.Event, 1)
	p := New(Config{HeartbeatInterval: time.Minute}, events, idleSince(time.Hour), nil,
		func() string { return "" },
		func() string { return "工作区有改动" },
	)
	p.beat(context.Background())
	select {
	case ev := <-events:
		if ev.Source != agent.SourceHeartbeat || !ev.Ephemeral {
			t.Fatalf("应为 Ephemeral 的心跳事件，得到 %+v", ev)
		}
		if !strings.Contains(ev.Text, "工作区有改动") || !strings.Contains(ev.Text, agent.Sentinel) {
			t.Fatalf("事件文本应含报告与哨兵：%s", ev.Text)
		}
	default:
		t.Fatal("有报告时应投事件")
	}
}

func TestBeatSkipsWhenBusyOrRecentlyActive(t *testing.T) {
	report := func() string { return "有事" }

	// turn 进行中 → 跳过。
	events := make(chan agent.Event, 1)
	p := New(Config{HeartbeatInterval: time.Minute}, events, idleSince(time.Hour),
		func() bool { return true }, report)
	p.beat(context.Background())
	if len(events) != 0 {
		t.Fatal("busy 时应跳过这一拍")
	}

	// 用户刚活跃过 → 跳过。
	p = New(Config{HeartbeatInterval: time.Minute}, events, idleSince(time.Second), nil, report)
	p.beat(context.Background())
	if len(events) != 0 {
		t.Fatal("用户刚活跃时应跳过这一拍")
	}
}

func TestBeatDoesNotBlockOnFullQueue(t *testing.T) {
	events := make(chan agent.Event, 1)
	events <- agent.Event{Source: agent.SourceUser, Text: "排队中"} // 占满
	p := New(Config{HeartbeatInterval: time.Minute}, events, idleSince(time.Hour), nil,
		func() string { return "有事" })
	doneCh := make(chan struct{})
	go func() { p.beat(context.Background()); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("队列满时 beat 不应阻塞")
	}
	if len(events) != 1 {
		t.Fatal("队列满时这一拍应作废")
	}
}

func TestTodoCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "todo.md")
	c := TodoCheck(path)

	if c() != "" {
		t.Fatal("文件不存在应无报告")
	}
	os.WriteFile(path, []byte("  \n\t\n"), 0o644)
	if c() != "" {
		t.Fatal("仅空白应无报告")
	}
	os.WriteFile(path, []byte("- 修复 bug\n"), 0o644)
	if c() == "" {
		t.Fatal("待办非空应报告")
	}
}

func TestWorkspaceCheck(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0o644)
	c := WorkspaceCheck(dir)

	if c() != "" {
		t.Fatal("首拍只建基线，不应报告")
	}
	if c() != "" {
		t.Fatal("无变化不应报告")
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new"), 0o644)
	if c() == "" {
		t.Fatal("新增文件应报告改动")
	}
	if c() != "" {
		t.Fatal("报告过后无新变化，不应再报告")
	}
	// 隐藏目录里的变化不计入。
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "x"), []byte("x"), 0o644)
	if c() != "" {
		t.Fatal("隐藏目录的变化应被忽略")
	}
}
