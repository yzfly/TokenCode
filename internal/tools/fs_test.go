package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustExec(t *testing.T, tool Tool, args map[string]any) string {
	t.Helper()
	in, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return out
}

// writeTree 铺一棵测试文件树。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"b.txt": "hi", "sub/c.txt": "x"})

	out := mustExec(t, Ls(), map[string]any{"path": dir})
	if !strings.Contains(out, "sub/") || !strings.Contains(out, "b.txt") {
		t.Errorf("ls output missing entries:\n%s", out)
	}
	// 目录排在文件前。
	if strings.Index(out, "sub/") > strings.Index(out, "b.txt") {
		t.Errorf("dirs should come first:\n%s", out)
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":         "package a",
		"pkg/b.go":     "package b",
		"pkg/sub/c.go": "package c",
		"pkg/d.txt":    "text",
	})

	// '*.go' 只匹配顶层。
	out := mustExec(t, Glob(), map[string]any{"pattern": "*.go", "path": dir})
	if !strings.Contains(out, "a.go") || strings.Contains(out, "b.go") {
		t.Errorf("*.go should match top-level only:\n%s", out)
	}

	// '**/*.go' 递归匹配（含顶层）。
	out = mustExec(t, Glob(), map[string]any{"pattern": "**/*.go", "path": dir})
	for _, want := range []string{"a.go", filepath.Join("pkg", "b.go"), filepath.Join("pkg", "sub", "c.go")} {
		if !strings.Contains(out, want) {
			t.Errorf("**/*.go missing %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "d.txt") {
		t.Errorf("**/*.go must not match d.txt:\n%s", out)
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "pkg/a.go", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "pkg/sub/a.go", true},
		{"pkg/**/*.go", "pkg/a.go", true},
		{"pkg/**/*.go", "other/a.go", false},
		{"cmd/*/main.go", "cmd/tokencode/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":   "package a\nfunc TODO_Fix() {}\n",
		"b.txt":  "todo: later\n",
		"bin.go": "x\x00y", // 二进制嗅探应跳过
	})

	out := mustExec(t, Grep(), map[string]any{"pattern": "TODO", "path": dir})
	if !strings.Contains(out, "a.go:2") {
		t.Errorf("missing match a.go:2:\n%s", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("case-sensitive search must not match b.txt:\n%s", out)
	}

	// ignore_case + include 过滤。
	out = mustExec(t, Grep(), map[string]any{"pattern": "todo", "path": dir, "ignore_case": true, "include": "*.txt"})
	if !strings.Contains(out, "b.txt:1") || strings.Contains(out, "a.go") {
		t.Errorf("include filter wrong:\n%s", out)
	}

	// 无匹配给出可读提示。
	out = mustExec(t, Grep(), map[string]any{"pattern": "nonexistent_zzz", "path": dir})
	if !strings.Contains(out, "no matches") {
		t.Errorf("want 'no matches', got:\n%s", out)
	}
}

func TestFsToolsRespectRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ctx := WithRoot(context.Background(), root)

	for _, tc := range []struct {
		tool Tool
		args map[string]any
	}{
		{Ls(), map[string]any{"path": outside}},
		{Glob(), map[string]any{"pattern": "*", "path": outside}},
		{Grep(), map[string]any{"pattern": "x", "path": outside}},
	} {
		in, _ := json.Marshal(tc.args)
		if _, err := tc.tool.Execute(ctx, in); err == nil {
			t.Errorf("%s: escaping root must fail", tc.tool.Name())
		}
	}
}
