package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return tool.Execute(context.Background(), b)
}

func TestWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "hello.txt")

	if _, err := run(t, Write(), map[string]any{"path": p, "content": "line1\nline2\n"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	out, err := run(t, Read(), map[string]any{"path": p})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "1\tline1") || !strings.Contains(out, "2\tline2") {
		t.Fatalf("unexpected read output:\n%s", out)
	}
}

func TestReadOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, Read(), map[string]any{"path": p, "offset": 2, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2\tb") || !strings.Contains(out, "3\tc") {
		t.Fatalf("offset/limit wrong:\n%s", out)
	}
	if strings.Contains(out, "\ta\n") || strings.Contains(out, "\td\n") {
		t.Fatalf("offset/limit leaked lines:\n%s", out)
	}
}

func TestReadMissing(t *testing.T) {
	if _, err := run(t, Read(), map[string]any{"path": "/no/such/file/xyz-123"}); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEdit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Edit(), map[string]any{"path": p, "old_string": "world", "new_string": "gong"}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "hello gong\n" {
		t.Fatalf("edit result wrong: %q", got)
	}
}

func TestEditNotUnique(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("x x x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Edit(), map[string]any{"path": p, "old_string": "x", "new_string": "y"}); err == nil {
		t.Fatal("expected non-unique error")
	}
	if _, err := run(t, Edit(), map[string]any{"path": p, "old_string": "x", "new_string": "y", "replace_all": true}); err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "y y y" {
		t.Fatalf("replace_all wrong: %q", got)
	}
}

func TestEditNotFound(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, Edit(), map[string]any{"path": p, "old_string": "zzz", "new_string": "y"}); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestBash(t *testing.T) {
	out, err := run(t, Bash(), map[string]any{"command": "echo hi"})
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "[exit code: 0]") {
		t.Fatalf("bash output wrong:\n%s", out)
	}
}

func TestBashExitCode(t *testing.T) {
	out, err := run(t, Bash(), map[string]any{"command": "exit 3"})
	if err != nil {
		t.Fatalf("bash err: %v", err)
	}
	if !strings.Contains(out, "[exit code: 3]") {
		t.Fatalf("expected exit 3:\n%s", out)
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(Read(), Write(), Edit(), Bash())
	if len(r.List()) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(r.List()))
	}
	if _, ok := r.Get("bash"); !ok {
		t.Fatal("bash not found")
	}
	if _, err := r.Execute(context.Background(), "nope", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}
