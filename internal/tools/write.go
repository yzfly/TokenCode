package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type writeTool struct{}

// Write 返回写文件的工具（创建或整体覆盖）。
func Write() Tool { return writeTool{} }

func (writeTool) Name() string { return "write" }

func (writeTool) Description() string {
	return "Write content to a file, creating it (and parent directories) or overwriting it entirely."
}

func (writeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path to the file"},
			"content": map[string]any{"type": "string", "description": "Full content to write"},
		},
		"required": []string{"path", "content"},
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (writeTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a writeArgs
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if dir := filepath.Dir(a.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
}
