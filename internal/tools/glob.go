package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// globMaxResults 是单次 glob 返回的文件数上限。
const globMaxResults = 200

type globTool struct{}

// Glob 返回按通配模式找文件的工具（只读免确认）。
func Glob() Tool { return globTool{} }

func (globTool) Name() string { return "glob" }

func (globTool) Description() string {
	return "Find files by glob pattern, newest first. '*' matches within one path segment, '**' matches any depth: use '*.go' for top-level files and '**/*.go' for recursive search. Skips .git."
}

func (globTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern, e.g. '**/*.go' or 'cmd/*/main.go'"},
			"path":    map[string]any{"type": "string", "description": "Base directory to search from (default: working directory)"},
		},
		"required": []string{"pattern"},
	}
}

func (globTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	// 先验证模式本身合法，免得整棵树走完才报错。
	if _, err := path.Match(strings.ReplaceAll(a.Pattern, "**", "*"), ""); err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	base, err := resolvePath(ctx, a.Path)
	if err != nil {
		return "", err
	}

	type hit struct {
		rel string
		mod time.Time
	}
	var hits []hit
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的目录跳过，不让局部错误毁掉整次搜索
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		if !matchGlob(a.Pattern, filepath.ToSlash(rel)) {
			return nil
		}
		mod := time.Time{}
		if info, err := d.Info(); err == nil {
			mod = info.ModTime()
		}
		hits = append(hits, hit{rel: rel, mod: mod})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no files match %q under %s", a.Pattern, base), nil
	}
	// 最近改动在前——刚动过的文件通常就是要找的。
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })

	var b strings.Builder
	for i, h := range hits {
		if i >= globMaxResults {
			fmt.Fprintf(&b, "... (%d more files truncated)\n", len(hits)-i)
			break
		}
		b.WriteString(h.rel + "\n")
	}
	return b.String(), nil
}

// matchGlob 按 / 分段匹配：'**' 匹配任意多段（含零段），其余段用 path.Match。
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(ps, ns []string) bool {
	for len(ps) > 0 {
		if ps[0] == "**" {
			for i := 0; i <= len(ns); i++ {
				if matchSegs(ps[1:], ns[i:]) {
					return true
				}
			}
			return false
		}
		if len(ns) == 0 {
			return false
		}
		if ok, err := path.Match(ps[0], ns[0]); err != nil || !ok {
			return false
		}
		ps, ns = ps[1:], ns[1:]
	}
	return len(ns) == 0
}
