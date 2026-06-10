package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withWorkspace 在测试期间临时开启工作空间隔离，结束后还原。
func withWorkspace(t *testing.T, root string) {
	t.Helper()
	old := workspaceRoot
	if err := SetWorkspace(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { workspaceRoot = old })
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWorkspaceGuard(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	inFile := filepath.Join(ws, "in.txt")
	outFile := filepath.Join(outside, "out.txt")
	for _, p := range []string{inFile, outFile} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withWorkspace(t, ws)

	ctx := context.Background()

	// 空间内读写放行。
	if _, err := Read().Execute(ctx, mustJSON(t, map[string]any{"path": inFile})); err != nil {
		t.Errorf("read inside: %v", err)
	}
	if _, err := Write().Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(ws, "new.txt"), "content": "y"})); err != nil {
		t.Errorf("write inside: %v", err)
	}

	// 空间外一律拒绝：读、写、改。
	if _, err := Read().Execute(ctx, mustJSON(t, map[string]any{"path": outFile})); err == nil || !strings.Contains(err.Error(), "工作空间") {
		t.Errorf("read outside should be rejected, got %v", err)
	}
	if _, err := Write().Execute(ctx, mustJSON(t, map[string]any{"path": outFile, "content": "y"})); err == nil || !strings.Contains(err.Error(), "工作空间") {
		t.Errorf("write outside should be rejected, got %v", err)
	}
	if _, err := Edit().Execute(ctx, mustJSON(t, map[string]any{"path": outFile, "old_string": "x", "new_string": "y"})); err == nil || !strings.Contains(err.Error(), "工作空间") {
		t.Errorf("edit outside should be rejected, got %v", err)
	}

	// 相对路径逃逸（../）也拦截。
	esc := filepath.Join(ws, "..", filepath.Base(outside), "out.txt")
	if _, err := Read().Execute(ctx, mustJSON(t, map[string]any{"path": esc})); err == nil {
		t.Error("dot-dot escape should be rejected")
	}
}

func TestWorkspaceGuardSymlinkEscape(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 空间内放一个指向外部目录的符号链接。
	link := filepath.Join(ws, "leak")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	withWorkspace(t, ws)

	ctx := context.Background()
	if _, err := Read().Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(link, "secret.txt")})); err == nil {
		t.Error("symlink read escape should be rejected")
	}
	// 经符号链接在外部创建新文件（目标不存在）也要拦住。
	if _, err := Write().Execute(ctx, mustJSON(t, map[string]any{"path": filepath.Join(link, "evil.txt"), "content": "x"})); err == nil {
		t.Error("symlink write escape should be rejected")
	}
}

func TestRegistryRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "in.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "out.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(Read(), Write(), Edit(), Bash())
	reg.SetRoot(root)
	ctx := context.Background()

	// 相对路径基于根解析。
	if _, err := reg.Execute(ctx, "read", mustJSON(t, map[string]any{"path": "in.txt"})); err != nil {
		t.Errorf("read relative inside root: %v", err)
	}
	if _, err := reg.Execute(ctx, "write", mustJSON(t, map[string]any{"path": "sub/new.txt", "content": "y"})); err != nil {
		t.Errorf("write relative inside root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "new.txt")); err != nil {
		t.Errorf("relative write should land under root: %v", err)
	}

	// 根之外的绝对路径与 ../ 逃逸一律拒绝。
	if _, err := reg.Execute(ctx, "read", mustJSON(t, map[string]any{"path": filepath.Join(outside, "out.txt")})); err == nil {
		t.Error("absolute path outside root should be rejected")
	}
	if _, err := reg.Execute(ctx, "write", mustJSON(t, map[string]any{"path": "../escape.txt", "content": "y"})); err == nil {
		t.Error("dot-dot escape from root should be rejected")
	}

	// bash 在根目录下执行。
	out, err := reg.Execute(ctx, "bash", mustJSON(t, map[string]any{"command": "pwd"}))
	if err != nil {
		t.Fatalf("bash under root: %v", err)
	}
	want := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		want = r
	}
	if !strings.Contains(out, want) {
		t.Errorf("bash cwd = %q, want under %q", out, want)
	}

	// 未绑定根的注册表行为不变（绝对路径自由访问）。
	free := NewRegistry(Read())
	if _, err := free.Execute(ctx, "read", mustJSON(t, map[string]any{"path": filepath.Join(outside, "out.txt")})); err != nil {
		t.Errorf("rootless registry should allow absolute path: %v", err)
	}
}

func TestWorkspaceGuardDisabled(t *testing.T) {
	withWorkspace(t, t.TempDir())
	workspaceRoot = "" // 显式关闭
	f := filepath.Join(t.TempDir(), "free.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read().Execute(context.Background(), mustJSON(t, map[string]any{"path": f})); err != nil {
		t.Errorf("disabled guard should allow anything: %v", err)
	}
}
