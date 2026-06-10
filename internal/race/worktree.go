package race

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// worktree 是一个 racer 的隔离写空间：从 HEAD 切出的独立分支 + git worktree。
// 共享对象库，创建快、磁盘省——这是 N 可以到 1000 的物质基础。
type worktree struct {
	Repo   string // 主仓库根
	Dir    string // worktree 目录（系统临时目录下，不污染仓库）
	Branch string
}

// RepoRoot 返回 dir 所在 git 仓库的根目录（非仓库时报错）。
func RepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("不是 git 仓库（竞赛模式依赖 git worktree）: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// addWorktree 为第 i 个 racer 创建写空间：分支 tokencode/race-<runID>-<i>。
func addWorktree(ctx context.Context, repo, baseDir, runID string, i int) (worktree, error) {
	w := worktree{
		Repo:   repo,
		Dir:    filepath.Join(baseDir, fmt.Sprintf("agent-%d", i)),
		Branch: fmt.Sprintf("tokencode/race-%s-%d", runID, i),
	}
	if _, err := git(ctx, repo, "worktree", "add", "-b", w.Branch, w.Dir, "HEAD"); err != nil {
		return worktree{}, fmt.Errorf("创建 worktree #%d: %w", i, err)
	}
	return w, nil
}

// Diff 收集 worktree 相对 HEAD 的全部改动（含新文件，二进制安全）。
// add -N 把未跟踪文件登记为 intent-to-add，diff 才看得见它们。
func (w worktree) Diff(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Dir, "add", "-A", "-N"); err != nil {
		return "", err
	}
	return git(ctx, w.Dir, "diff", "--binary", "HEAD")
}

// DiffStat 返回改动统计（文件数与增删行，给排行榜显示）。
func (w worktree) DiffStat(ctx context.Context) string {
	out, err := git(ctx, w.Dir, "diff", "--stat", "HEAD")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]) // 末行是 "N files changed, ..."
}

// RunCheck 在 worktree 里跑客观校验命令，返回输出；非零退出即 err。
func (w worktree) RunCheck(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = w.Dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Remove 清理 worktree；keepBranch=false 时连分支一起删（败者），
// 冠军保留分支以便追溯与手动 merge。
func (w worktree) Remove(ctx context.Context, keepBranch bool) error {
	_, err := git(ctx, w.Repo, "worktree", "remove", "--force", w.Dir)
	if !keepBranch {
		if _, berr := git(ctx, w.Repo, "branch", "-D", w.Branch); err == nil {
			err = berr
		}
	}
	return err
}

// Apply 把冠军 diff 应用到主工作区（保持未提交状态，由用户审查后自行 commit）。
func Apply(ctx context.Context, repo, diff string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "apply", "--binary", "--whitespace=nowarn", "-")
	cmd.Stdin = strings.NewReader(diff)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git apply 失败（主工作区可能与竞赛起点有冲突）: %s", strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// git 在 dir 下执行一条 git 命令，返回 stdout。
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	// worktree 操作不该被仓库级 hooks 或模板干扰。
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}
