// Package skill 实现 Agent Skills 开放标准的最小加载器：发现 SKILL.md、
// 解析 frontmatter、按 /技能名 调用时展开正文。兼容 Claude Code / Codex 的
// 技能目录（.claude/skills、.agents/skills），生态零成本互通。
//
// 渐进披露从第一天遵守：启动只读 frontmatter（name/description 进 /skills
// 列表与命令路由），正文在被调用时才读盘展开——技能再多也不拖慢启动。
package skill

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Skill 是一个已发现的技能。Body 懒加载：列表阶段为空，Expand 时才读。
type Skill struct {
	Name        string
	Description string
	Path        string // SKILL.md 绝对路径
	Source      string // 来源目录标签（project / user / claude / agents）
}

// Discover 扫描各级技能目录，返回按名去重后的技能列表（先到先得，
// 项目级压过用户级）。目录不存在不是错误。
func Discover(cwd string) []Skill {
	home, _ := os.UserHomeDir()
	roots := []struct{ dir, label string }{
		{filepath.Join(cwd, ".tokencode", "skills"), "project"},
		{filepath.Join(cwd, ".claude", "skills"), "project(claude)"},
		{filepath.Join(cwd, ".agents", "skills"), "project(agents)"},
		{filepath.Join(home, ".config", "tokencode", "skills"), "user"},
		{filepath.Join(home, ".claude", "skills"), "user(claude)"},
	}

	seen := map[string]bool{}
	var out []Skill
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root.dir, e.Name(), "SKILL.md")
			s, err := readMeta(path)
			if err != nil {
				continue
			}
			if s.Name == "" {
				s.Name = e.Name()
			}
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			s.Source = root.label
			out = append(out, s)
		}
	}
	return out
}

// readMeta 只解析 frontmatter（不留正文在内存）。
func readMeta(path string) (Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, err
	}
	defer f.Close()

	s := Skill{Path: path}
	sc := bufio.NewScanner(f)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return s, nil // 无 frontmatter：名字取目录名，描述为空
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			s.Name = val
		case "description":
			s.Description = val
		}
	}
	return s, sc.Err()
}

// Expand 读取正文并代入参数：替换 $ARGUMENTS；正文没出现该占位符且
// 参数非空时，以 "ARGUMENTS: <args>" 追加（与 Claude Code 行为一致）。
func (s Skill) Expand(args string) (string, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("skill: 读取 %s: %w", s.Path, err)
	}
	body := stripFrontmatter(string(raw))
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args), nil
	}
	if args != "" {
		body = strings.TrimRight(body, "\n") + "\n\nARGUMENTS: " + args
	}
	return body, nil
}

// stripFrontmatter 去掉开头的 --- 块。
func stripFrontmatter(s string) string {
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return s
	}
	if _, after, ok := strings.Cut(rest, "\n---\n"); ok {
		return strings.TrimLeft(after, "\n")
	}
	if _, after, ok := strings.Cut(rest, "\n---"); ok {
		return strings.TrimLeft(after, "\n")
	}
	return s
}
