package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo 建一个有首次提交的临时 git 仓库。
func initRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	// macOS 等平台 TempDir 可能是符号链接，统一解析成真实路径再比对。
	if r, err := filepath.EvalSymlinks(repo); err == nil {
		repo = r
	}
	run := func(args ...string) {
		t.Helper()
		if _, err := gitOut(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return repo
}

// TestEnsureWorktree 创建、复用、目录被删后按既有分支重建。
func TestEnsureWorktree(t *testing.T) {
	repo := initRepo(t)

	dir, err := ensureWorktree(repo, "feat")
	if err != nil {
		t.Fatalf("创建: %v", err)
	}
	want := filepath.Join(repo, ".tokencode", "worktrees", "feat")
	if dir != want {
		t.Fatalf("目录应为 %s，得到 %s", want, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatalf("worktree 应含 HEAD 的文件: %v", err)
	}
	if br, err := gitOut(dir, "branch", "--show-current"); err != nil || br != "tokencode/wt-feat" {
		t.Fatalf("分支应为 tokencode/wt-feat，得到 %q（err=%v）", br, err)
	}

	// 同名复用：第二次返回同一目录，不报错。
	dir2, err := ensureWorktree(repo, "feat")
	if err != nil || dir2 != dir {
		t.Fatalf("复用失败：dir=%s err=%v", dir2, err)
	}

	// 目录被手动删掉、分支还在：按既有分支重建。
	if _, err := gitOut(repo, "worktree", "remove", "--force", dir); err != nil {
		t.Fatalf("移除 worktree: %v", err)
	}
	dir3, err := ensureWorktree(repo, "feat")
	if err != nil || dir3 != dir {
		t.Fatalf("分支残留重建失败：dir=%s err=%v", dir3, err)
	}
}

// TestEnsureWorktreeErrors 非 git 目录与非法名字都应报错。
func TestEnsureWorktreeErrors(t *testing.T) {
	if _, err := ensureWorktree(t.TempDir(), "x"); err == nil || !strings.Contains(err.Error(), "git 仓库") {
		t.Fatalf("非 git 仓库应报错，得到 %v", err)
	}
	repo := initRepo(t)
	for _, bad := range []string{"", "a/b", "..", `a\b`} {
		if _, err := ensureWorktree(repo, bad); err == nil {
			t.Fatalf("名字 %q 应报错", bad)
		}
	}
}
