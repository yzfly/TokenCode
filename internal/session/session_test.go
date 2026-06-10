package session

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
)

// TestRoundTrip 验证 Create → Append → Load 的完整往返：
// meta、文本消息、tool_use/tool_result 都原样回来。
func TestRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	s, err := Create("/tmp/proj", "test-model")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	msgs := []llm.Message{
		{Role: llm.RoleUser, Text: "read a.txt"},
		{Role: llm.RoleAssistant, Text: "ok", ToolUses: []llm.ToolUse{
			{ID: "call_1", Name: "read", Input: json.RawMessage(`{"path":"a.txt"}`)},
		}},
		{Role: llm.RoleUser, ToolResults: []llm.ToolResult{
			{ToolUseID: "call_1", Content: "hi", IsError: false},
		}},
		{Role: llm.RoleAssistant, Text: "done"},
	}
	s.Append(msgs[:2])
	s.Append(msgs[2:])
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	meta, got, err := Load(s.ID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if meta.ID != s.ID() || meta.Cwd != "/tmp/proj" || meta.Model != "test-model" {
		t.Fatalf("meta wrong: %+v", meta)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(got))
	}
	if got[1].ToolUses[0].ID != "call_1" || string(got[1].ToolUses[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use wrong: %+v", got[1].ToolUses)
	}
	if got[2].ToolResults[0].ToolUseID != "call_1" || got[2].ToolResults[0].Content != "hi" {
		t.Fatalf("tool result wrong: %+v", got[2].ToolResults)
	}
}

// TestOpenAppends 验证 Open 后追加写进同一个文件，Load 能读全。
func TestOpenAppends(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	s, err := Create("/tmp/proj", "m")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.Append([]llm.Message{{Role: llm.RoleUser, Text: "one"}})
	s.Close()

	s2, err := Open(s.ID())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s2.Append([]llm.Message{{Role: llm.RoleAssistant, Text: "two"}})
	s2.Close()

	_, got, err := Load(s.ID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" {
		t.Fatalf("messages wrong: %+v", got)
	}
}

// TestLatest 验证按 cwd 取最近会话：不同 cwd 不串、时间序正确。
func TestLatest(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	a, _ := Create("/proj/a", "m")
	a.Close()
	time.Sleep(1100 * time.Millisecond) // id 粒度到秒，确保第二个 id 更大
	b, _ := Create("/proj/a", "m")
	b.Close()
	c, _ := Create("/proj/b", "m")
	c.Close()

	id, err := Latest("/proj/a")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if id != b.ID() {
		t.Fatalf("expected %s, got %s", b.ID(), id)
	}
	if _, err := Latest("/proj/none"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestLoadSkipsCorruptLine 验证半行损坏（崩溃尾巴）被跳过，其余照常读回。
func TestLoadSkipsCorruptLine(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	s, _ := Create("/p", "m")
	s.Append([]llm.Message{{Role: llm.RoleUser, Text: "ok"}})
	s.Close()

	f, err := os.OpenFile(pathForID(s.ID()), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"message","role":"assist`) // 写一半崩了
	f.Close()

	_, got, err := Load(s.ID())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Text != "ok" {
		t.Fatalf("messages wrong: %+v", got)
	}
}
