package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo 建一个带初始提交的临时 git 仓库，返回其路径。
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func gitExec(t *testing.T, tool Tool, dir string, args map[string]any) (string, error) {
	t.Helper()
	in, _ := json.Marshal(args)
	return tool.Execute(WithRoot(context.Background(), dir), in)
}

func TestGitStatusCleanAndDirty(t *testing.T) {
	dir := initRepo(t)

	out, err := gitExec(t, GitStatus(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "working tree clean") {
		t.Errorf("want clean marker:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = gitExec(t, GitStatus(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.txt") {
		t.Errorf("want untracked b.txt:\n%s", out)
	}
}

func TestGitDiffAndCommit(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := gitExec(t, GitDiff(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "+two") {
		t.Errorf("diff missing +two:\n%s", out)
	}

	out, err = gitExec(t, GitCommit(), dir, map[string]any{"message": "add two", "add_all": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "add two") {
		t.Errorf("commit output:\n%s", out)
	}

	// 提交后无 diff。
	out, err = gitExec(t, GitDiff(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no diff)" {
		t.Errorf("want '(no diff)', got:\n%s", out)
	}
}

func TestGitCommitRequiresMessage(t *testing.T) {
	dir := initRepo(t)
	if _, err := gitExec(t, GitCommit(), dir, map[string]any{"message": "  "}); err == nil {
		t.Fatal("empty message must fail")
	}
}
