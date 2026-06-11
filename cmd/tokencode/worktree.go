package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ensureWorktree 实现 -w <name>：在 cwd 所在 git 仓库下确保
// <repo>/.tokencode/worktrees/<name> 这个 worktree 存在（分支
// tokencode/wt-<name>，基于 HEAD），返回其目录。已存在同名目录直接复用；
// 分支还在但目录被删过时，用既有分支重建。非 git 仓库报错。
// 退出不自动删——用户手动 `git worktree remove` 清理。
func ensureWorktree(cwd, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", fmt.Errorf("-w 名字非法：%q（不能含路径分隔符）", name)
	}
	repo, err := gitOut(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("-w 需要在 git 仓库内使用：%w", err)
	}
	dir := filepath.Join(repo, ".tokencode", "worktrees", name)
	branch := "tokencode/wt-" + name

	if _, err := os.Stat(dir); err == nil {
		return dir, nil // 同名 worktree 已存在：复用
	}
	// 分支可能上次建过但目录被手动删了：有分支就直接挂载，否则从 HEAD 新建。
	if _, err := gitOut(repo, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		// 残留的 worktree 登记会挡住重建，先尽力清一次。
		_, _ = gitOut(repo, "worktree", "prune")
		if _, err := gitOut(repo, "worktree", "add", dir, branch); err != nil {
			return "", fmt.Errorf("挂载已有分支 %s: %w", branch, err)
		}
		return dir, nil
	}
	if _, err := gitOut(repo, "worktree", "add", "-b", branch, dir, "HEAD"); err != nil {
		return "", fmt.Errorf("创建 worktree %s: %w", name, err)
	}
	return dir, nil
}

// gitOut 在 dir 下执行一条 git 命令，返回去除首尾空白的 stdout。
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errBuf.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
