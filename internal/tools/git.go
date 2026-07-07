package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gitTimeout 是单条 git 命令的超时；gitMaxOutput 是 diff 输出截断上限。
const (
	gitTimeout   = 30 * time.Second
	gitMaxOutput = 30000
)

// runGit 在工具根（无根则进程 cwd）下执行一条 git 命令。
// 非零退出把 stderr 作为错误带回——喂给模型让它自己修正。
func runGit(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = rootFrom(cctx) // 空=继承进程 cwd，与 bash 同一语义
	out, err := cmd.CombinedOutput()
	res := strings.TrimRight(string(out), "\n")
	if err != nil {
		if res != "" {
			return "", fmt.Errorf("git %s: %s", args[0], res)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return res, nil
}

// ---- git_status ----

type gitStatusTool struct{}

// GitStatus 返回查看工作树状态的工具（只读免确认）。
func GitStatus() Tool { return gitStatusTool{} }

func (gitStatusTool) Name() string { return "git_status" }

func (gitStatusTool) Description() string {
	return "Show git working tree status: current branch plus changed/staged/untracked files (porcelain format)."
}

func (gitStatusTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (gitStatusTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	out, err := runGit(ctx, "status", "--porcelain=v1", "-b")
	if err != nil {
		return "", err
	}
	// porcelain 首行恒为分支信息；只剩它就是干净工作树。
	if strings.Count(out, "\n") == 0 {
		return out + "\n(working tree clean)", nil
	}
	return out, nil
}

// ---- git_diff ----

type gitDiffTool struct{}

// GitDiff 返回查看改动 diff 的工具（只读免确认）。
func GitDiff() Tool { return gitDiffTool{} }

func (gitDiffTool) Name() string { return "git_diff" }

func (gitDiffTool) Description() string {
	return "Show git diff of unstaged changes by default; set staged=true for the index, ref to diff against a commit/branch, path to limit scope."
}

func (gitDiffTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"staged": map[string]any{"type": "boolean", "description": "Diff the staged index instead of the working tree (optional)"},
			"ref":    map[string]any{"type": "string", "description": "Diff against this commit/branch, e.g. 'HEAD~1' or 'main' (optional)"},
			"path":   map[string]any{"type": "string", "description": "Limit the diff to this file or directory (optional)"},
		},
	}
}

func (gitDiffTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Staged bool   `json:"staged"`
		Ref    string `json:"ref"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args := []string{"diff"}
	if a.Staged {
		args = append(args, "--staged")
	}
	if a.Ref != "" {
		args = append(args, a.Ref)
	}
	if a.Path != "" {
		args = append(args, "--", a.Path)
	}
	out, err := runGit(ctx, args...)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "(no diff)", nil
	}
	if len(out) > gitMaxOutput {
		out = out[:gitMaxOutput] + "\n... (diff truncated; narrow with the path argument)"
	}
	return out, nil
}

// ---- git_commit ----

type gitCommitTool struct{}

// GitCommit 返回提交改动的工具（写操作，走确认）。
func GitCommit() Tool { return gitCommitTool{} }

func (gitCommitTool) Name() string { return "git_commit" }

func (gitCommitTool) Description() string {
	return "Create a git commit with the given message. Commits what is already staged by default; pass paths to stage specific files first, or add_all=true to stage everything."
}

func (gitCommitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "description": "Commit message"},
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Files to 'git add' before committing (optional)"},
			"add_all": map[string]any{"type": "boolean", "description": "Stage all changes (git add -A) before committing (optional)"},
		},
		"required": []string{"message"},
	}
}

func (gitCommitTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
		AddAll  bool     `json:"add_all"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Message) == "" {
		return "", fmt.Errorf("message is required")
	}
	switch {
	case a.AddAll:
		if _, err := runGit(ctx, "add", "-A"); err != nil {
			return "", err
		}
	case len(a.Paths) > 0:
		if _, err := runGit(ctx, append([]string{"add", "--"}, a.Paths...)...); err != nil {
			return "", err
		}
	}
	return runGit(ctx, "commit", "-m", a.Message)
}
