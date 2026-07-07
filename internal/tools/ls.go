package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// lsMaxEntries 是单次列目录返回的条目上限，防止超大目录撑爆上下文。
const lsMaxEntries = 500

type lsTool struct{}

// Ls 返回列目录工具（只读免确认）。
func Ls() Tool { return lsTool{} }

func (lsTool) Name() string { return "ls" }

func (lsTool) Description() string {
	return "List entries of a directory. Directories end with '/', files show their size. Defaults to the working directory."
}

func (lsTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory to list (default: working directory)"},
		},
	}
}

func (lsTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	dir, err := resolvePath(ctx, a.Path)
	if err != nil {
		return "", err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	// 目录在前、各自按名排序——扫一眼就能分清结构和文件。
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return ents[i].Name() < ents[j].Name()
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", dir)
	n := 0
	for _, e := range ents {
		if n >= lsMaxEntries {
			fmt.Fprintf(&b, "... (%d more entries truncated)\n", len(ents)-n)
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "  %s/\n", e.Name())
		} else if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "  %s  (%d bytes)\n", e.Name(), info.Size())
		} else {
			fmt.Fprintf(&b, "  %s\n", e.Name())
		}
		n++
	}
	if n == 0 {
		return dir + "\n(empty directory)", nil
	}
	return b.String(), nil
}
