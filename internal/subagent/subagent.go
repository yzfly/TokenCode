// Package subagent 实现子代理：模型通过 agent 工具把任务委托给一个
// 全新隔离上下文的子 agent，子代理跑完整个 tool-use 循环后只把最终文本
// 返回给主 agent——中间的探索与工具噪音留在子代理自己的上下文里。
//
// 与 Claude Code 对齐的语义：
//   - 一条 assistant 消息里的多个 agent 调用并行执行（tools.Concurrent）；
//   - 子代理禁止再生成子代理（子注册表剔除 agent/workflow）；
//   - 自定义代理从 .tokencode/agents、.claude/agents 等目录发现，
//     frontmatter 定义 name/description/tools/model，正文是系统提示。
package subagent

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Def 是一个子代理类型的定义。
type Def struct {
	Name        string
	Description string   // 模型据此决定何时委托（写进 agent 工具描述）
	Tools       []string // 允许的工具名；空=默认（全部，剔除 agent/workflow）
	Model       string   // 模型覆盖（别名或 provider/model-id）；空=继承主 agent
	Prompt      string   // 系统提示（文件正文）；内置类型直接给字符串
	Source      string   // 来源标签（builtin / project / user / ...）
}

// Builtins 返回内置子代理类型。
// explore 偏只读侦察（read+bash），general-purpose 全工具。
func Builtins() []Def {
	return []Def{
		{
			Name:        "explore",
			Description: "Read-only scout: locating files, searching code, answering codebase questions. Fast, never modifies anything. Prefer this for any pure-discovery task.",
			Tools:       []string{"read", "bash"},
			Prompt: "You are a read-only exploration sub-agent. Find what was asked " +
				"(files, code, structure, facts) using read and read-only shell commands " +
				"(ls, grep, find, git log...). NEVER modify anything: no file writes, no " +
				"state-changing commands. Report findings concisely with file paths and line numbers.",
			Source: "builtin",
		},
		{
			Name:        "general-purpose",
			Description: "Full-capability agent for delegated multi-step tasks: can read, write, edit and run commands. Use for self-contained subtasks whose details don't need to flow back.",
			Prompt: "You are a general-purpose sub-agent. Complete the delegated task " +
				"end to end using your tools, then report the outcome concisely.",
			Source: "builtin",
		},
	}
}

// Discover 返回内置 + 各级目录发现的自定义子代理，按名去重（先到先得，
// 项目级压过用户级；自定义同名可覆盖内置）。目录不存在不是错误。
// 与技能不同，代理定义文件是目录下的平铺 *.md（兼容 Claude Code 的 .claude/agents/）。
func Discover(cwd string) []Def {
	home, _ := os.UserHomeDir()
	roots := []struct{ dir, label string }{
		{filepath.Join(cwd, ".tokencode", "agents"), "project"},
		{filepath.Join(cwd, ".claude", "agents"), "project(claude)"},
		{filepath.Join(home, ".config", "tokencode", "agents"), "user"},
		{filepath.Join(home, ".claude", "agents"), "user(claude)"},
	}

	seen := map[string]bool{}
	var out []Def
	for _, root := range roots {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			d, err := readDef(filepath.Join(root.dir, e.Name()))
			if err != nil {
				continue
			}
			if d.Name == "" {
				d.Name = strings.TrimSuffix(e.Name(), ".md")
			}
			if seen[d.Name] {
				continue
			}
			seen[d.Name] = true
			d.Source = root.label
			out = append(out, d)
		}
	}
	// 内置垫底：同名自定义已占位则跳过。
	for _, b := range Builtins() {
		if !seen[b.Name] {
			seen[b.Name] = true
			out = append(out, b)
		}
	}
	return out
}

// readDef 解析一个代理定义文件：frontmatter（name/description/tools/model）+ 正文。
// 文件都很小，整体读入（无技能那种懒加载的必要）。
func readDef(path string) (Def, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Def{}, err
	}
	d := Def{}
	body := string(raw)
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		fm := rest
		if head, after, ok := strings.Cut(rest, "\n---"); ok {
			fm, body = head, strings.TrimLeft(strings.TrimPrefix(after, "\n"), "\n")
		}
		sc := bufio.NewScanner(strings.NewReader(fm))
		for sc.Scan() {
			key, val, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			val = strings.Trim(strings.TrimSpace(val), `"'`)
			switch strings.TrimSpace(key) {
			case "name":
				d.Name = val
			case "description":
				d.Description = val
			case "model":
				d.Model = val
			case "tools":
				for _, t := range strings.Split(val, ",") {
					if t = strings.TrimSpace(t); t != "" {
						d.Tools = append(d.Tools, t)
					}
				}
			}
		}
	}
	d.Prompt = strings.TrimSpace(body)
	return d, nil
}
