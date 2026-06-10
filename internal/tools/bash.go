package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type bashTool struct {
	timeout time.Duration
}

// Bash 返回执行 shell 命令的工具。
func Bash() Tool { return bashTool{timeout: 120 * time.Second} }

func (bashTool) Name() string { return "bash" }

func (bashTool) Description() string {
	return "Run a shell command via 'sh -c' in the current working directory. Returns combined stdout and stderr plus the exit code."
}

func (bashTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "The shell command to run"},
		},
		"required": []string{"command"},
	}
}

type bashArgs struct {
	Command string `json:"command"`
}

func (b bashTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a bashArgs
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("command is required")
	}

	to := b.timeout
	if to <= 0 {
		to = 120 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	out, err := exec.CommandContext(cctx, "sh", "-c", a.Command).CombinedOutput()
	res := string(out)

	if cctx.Err() == context.DeadlineExceeded {
		return res + fmt.Sprintf("\n[timed out after %s]", to), nil
	}
	// 命令非零退出不当作工具错误：把退出码喂回给模型，让它自己判断。
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			exit = ee.ExitCode()
		} else {
			return res, err
		}
	}
	return res + fmt.Sprintf("\n[exit code: %d]", exit), nil
}

// asExitError 是 errors.As 的小封装，避免在调用处引入额外 import。
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
