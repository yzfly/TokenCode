package tui

import (
	"strings"
	"testing"
)

func TestShellCtxInjection(t *testing.T) {
	m := model{}
	m.shellCtx = append(m.shellCtx, shellCtxBlock("ls", "a.txt\nb.txt\n", 0))
	m.shellCtx = append(m.shellCtx, shellCtxBlock("false", "", 1))

	out := m.takeShellCtx("这两个文件是干嘛的")
	if !strings.Contains(out, "<shell-input>ls</shell-input>") {
		t.Errorf("missing shell-input block: %q", out)
	}
	if !strings.Contains(out, "a.txt") {
		t.Errorf("missing output: %q", out)
	}
	if !strings.Contains(out, `exit="1"`) {
		t.Errorf("missing exit code: %q", out)
	}
	if !strings.HasSuffix(out, "这两个文件是干嘛的") {
		t.Errorf("user text must come last: %q", out)
	}
	if len(m.shellCtx) != 0 {
		t.Error("buffer should be drained")
	}
	// 无缓存：原样返回。
	if got := m.takeShellCtx("x"); got != "x" {
		t.Errorf("no-op expected, got %q", got)
	}
}

func TestShellCtxBlockTruncates(t *testing.T) {
	long := strings.Repeat("y", shellCtxCap+100)
	block := shellCtxBlock("cat big", long, 0)
	if len(block) > shellCtxCap+200 {
		t.Errorf("block not truncated: %d bytes", len(block))
	}
	if !strings.Contains(block, "已截断") {
		t.Error("missing truncation note")
	}
}
