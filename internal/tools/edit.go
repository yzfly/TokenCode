package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type editTool struct{}

// Edit 返回精确字符串替换的工具。
func Edit() Tool { return editTool{} }

func (editTool) Name() string { return "edit" }

func (editTool) Description() string {
	return "Replace an exact string in a file. old_string must match exactly and be unique unless replace_all is true."
}

func (editTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":        map[string]any{"type": "string", "description": "Path to the file"},
			"old_string":  map[string]any{"type": "string", "description": "Exact text to replace"},
			"new_string":  map[string]any{"type": "string", "description": "Replacement text"},
			"replace_all": map[string]any{"type": "boolean", "description": "Replace all occurrences (default false)"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (editTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a editArgs
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if a.OldString == a.NewString {
		return "", fmt.Errorf("old_string and new_string are identical")
	}
	path, err := resolvePath(ctx, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(data)
	n := strings.Count(content, a.OldString)
	if n == 0 {
		return "", fmt.Errorf("old_string not found in %s", a.Path)
	}
	if n > 1 && !a.ReplaceAll {
		return "", fmt.Errorf("old_string is not unique (%d matches); add more context or set replace_all", n)
	}

	repl := 1
	out := strings.Replace(content, a.OldString, a.NewString, 1)
	if a.ReplaceAll {
		repl = n
		out = strings.ReplaceAll(content, a.OldString, a.NewString)
	}
	notifyCheckpoint(ctx, path) // 校验都过了才快照，失败的 edit 不留检查点记录
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", a.Path, repl), nil
}
