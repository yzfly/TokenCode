package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	grepMaxMatches  = 100     // 返回的匹配行上限
	grepMaxFileSize = 1 << 20 // 超过 1MB 的文件跳过
	grepMaxLineLen  = 250     // 单行截断长度
	grepSniffLen    = 512     // 二进制嗅探读取的字节数
)

type grepTool struct{}

// Grep 返回按正则搜文件内容的工具（只读免确认）。
func Grep() Tool { return grepTool{} }

func (grepTool) Name() string { return "grep" }

func (grepTool) Description() string {
	return "Search file contents with a Go regular expression. Returns 'path:line: text' matches. Skips .git, binary files and files over 1MB."
}

func (grepTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":     map[string]any{"type": "string", "description": "Go regexp to search for"},
			"path":        map[string]any{"type": "string", "description": "File or directory to search (default: working directory)"},
			"include":     map[string]any{"type": "string", "description": "Only search files whose name matches this glob, e.g. '*.go' (optional)"},
			"ignore_case": map[string]any{"type": "boolean", "description": "Case-insensitive search (optional)"},
		},
		"required": []string{"pattern"},
	}
}

func (grepTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Include    string `json:"include"`
		IgnoreCase bool   `json:"ignore_case"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	base, err := resolvePath(ctx, a.Path)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	matches, truncated := 0, false
	search := func(p, rel string) {
		if matches >= grepMaxMatches {
			truncated = true
			return
		}
		matches += grepFile(&b, re, p, rel, grepMaxMatches-matches)
	}

	info, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		search(base, filepath.Base(base))
	} else {
		err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if truncated || matches >= grepMaxMatches {
				truncated = true
				return filepath.SkipAll
			}
			if a.Include != "" {
				if ok, err := path.Match(a.Include, d.Name()); err != nil || !ok {
					return nil
				}
			}
			if fi, err := d.Info(); err != nil || fi.Size() > grepMaxFileSize {
				return nil
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				rel = p
			}
			search(p, rel)
			return nil
		})
		if err != nil {
			return "", err
		}
	}

	if matches == 0 {
		return fmt.Sprintf("no matches for %q under %s", a.Pattern, base), nil
	}
	if truncated {
		fmt.Fprintf(&b, "... (results truncated at %d matches)\n", grepMaxMatches)
	}
	return b.String(), nil
}

// grepFile 在单个文件里搜索，把 "rel:line: text" 追加进 b，返回追加的行数。
// 二进制文件（前 512 字节含 NUL）直接跳过。
func grepFile(b *strings.Builder, re *regexp.Regexp, path, rel string, limit int) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	sniff := make([]byte, grepSniffLen)
	n, _ := f.Read(sniff)
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return 0
	}
	if _, err := f.Seek(0, 0); err != nil {
		return 0
	}

	found := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), grepMaxFileSize)
	for line := 1; sc.Scan(); line++ {
		if found >= limit {
			break
		}
		text := sc.Text()
		if !re.MatchString(text) {
			continue
		}
		if rs := []rune(text); len(rs) > grepMaxLineLen {
			text = string(rs[:grepMaxLineLen]) + "…"
		}
		fmt.Fprintf(b, "%s:%d: %s\n", rel, line, strings.TrimRight(text, " \t"))
		found++
	}
	return found
}
