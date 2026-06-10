package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type readTool struct{}

// Read 返回读取文件的工具。
func Read() Tool { return readTool{} }

func (readTool) Name() string { return "read" }

func (readTool) Description() string {
	return "Read a text file from the local filesystem. Returns the content with line numbers. Use offset/limit for large files."
}

func (readTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "Path to the file"},
			"offset": map[string]any{"type": "integer", "description": "1-based line to start from (optional)"},
			"limit":  map[string]any{"type": "integer", "description": "Max number of lines to read (optional)"},
		},
		"required": []string{"path"},
	}
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (readTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a readArgs
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	path, err := resolvePath(ctx, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	// 文件以换行结尾时 Split 会多出一个空串，去掉避免凭空多一行。
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if b.Len() == 0 {
		return "(empty)", nil
	}
	return b.String(), nil
}
