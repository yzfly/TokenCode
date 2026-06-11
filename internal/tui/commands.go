package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yzfly/tokencode/internal/usage"
)

// command 是注册表里的一条命令（需求文档 REQ-0）。
// /help、/ 补全菜单、分发逻辑都从同一张表生成，三处永远一致。
type command struct {
	name    string
	aliases []string
	argHint string // "<必填> [可选]" 记法，空=无参数
	summary string // 单行摘要
	source  string // ""=内置；"skill"=技能
	run     func(m model, args string) (tea.Model, tea.Cmd)
}

// commands 返回当前会话的全部命令：内置 + 技能（动态源）。
// 纯内存组装，渲染/补全调用它不触发任何磁盘 IO。
func (m model) commands() []command {
	cmds := []command{
		{name: "help", summary: "显示帮助与全部命令",
			run: func(m model, _ string) (tea.Model, tea.Cmd) {
				m.emit(transItem{kind: tNote, text: m.helpText()})
				return m, nil
			}},
		{name: "model", argHint: "[别名|provider/model-id]", summary: "查看或热切换模型",
			run: func(m model, args string) (tea.Model, tea.Cmd) { return m.cmdModel(args) }},
		{name: "skills", summary: "列出已发现的技能",
			run: func(m model, _ string) (tea.Model, tea.Cmd) {
				m.emit(transItem{kind: tNote, text: m.skillsText()})
				return m, nil
			}},
		{name: "mcp", argHint: "[reconnect <server>]", summary: "MCP server 状态与重连",
			run: func(m model, args string) (tea.Model, tea.Cmd) { return m.cmdMCP(args) }},
		{name: "race", argHint: "<N> <任务> | apply | discard", summary: "并行竞赛：N 个 agent 隔离解题，裁判择优",
			run: func(m model, args string) (tea.Model, tea.Cmd) { return m.cmdRace(args) }},
		{name: "usage", aliases: []string{"cost", "stats"}, summary: "token 用量：本月/今日合计与排行",
			run: func(m model, _ string) (tea.Model, tea.Cmd) {
				m.emit(transItem{kind: tNote, text: usageText(time.Now())})
				return m, nil
			}},
		{name: "compact", argHint: "[侧重点]", summary: "压缩历史上下文为摘要（长会话续命）",
			run: func(m model, args string) (tea.Model, tea.Cmd) { return m.cmdCompact(args) }},
		{name: "context", summary: "上下文用量：估算 tokens、消息占比与压缩余量",
			run: func(m model, _ string) (tea.Model, tea.Cmd) {
				m.emit(transItem{kind: tNote, text: m.contextText()})
				return m, nil
			}},
		{name: "agents", summary: "列出可用的子代理类型",
			run: func(m model, _ string) (tea.Model, tea.Cmd) {
				m.emit(transItem{kind: tNote, text: m.agentsText()})
				return m, nil
			}},
		{name: "plan", summary: "切到只读模式",
			run: func(m model, _ string) (tea.Model, tea.Cmd) { return m.setMode("plan") }},
		{name: "review", summary: "切到逐次确认模式（默认）",
			run: func(m model, _ string) (tea.Model, tea.Cmd) { return m.setMode("review") }},
		{name: "auto", summary: "切到小模型自动裁决模式",
			run: func(m model, _ string) (tea.Model, tea.Cmd) { return m.setMode("auto") }},
		{name: "yolo", summary: "切到全放行模式",
			run: func(m model, _ string) (tea.Model, tea.Cmd) { return m.setMode("yolo") }},
		{name: "exit", aliases: []string{"quit"}, summary: "退出（同 Ctrl-D）",
			run: func(m model, _ string) (tea.Model, tea.Cmd) { return m, tea.Quit }},
	}
	for _, s := range m.skills {
		s := s
		desc := s.Description
		if desc == "" {
			desc = "（无描述）"
		}
		cmds = append(cmds, command{
			name: s.Name, argHint: "[参数]", summary: desc, source: "skill",
			run: func(m model, args string) (tea.Model, tea.Cmd) { return m.runSkill(s.Name, args) },
		})
	}
	return cmds
}

// lookupCommand 按名字或别名查命令。
func lookupCommand(cmds []command, name string) (command, bool) {
	for _, c := range cmds {
		if c.name == name {
			return c, true
		}
		for _, a := range c.aliases {
			if a == name {
				return c, true
			}
		}
	}
	return command{}, false
}

// runCommand 分发一条以 / 开头的输入（REQ-5：未知命令不发模型，给就近建议；
// // 开头是转义——剥掉一个斜杠后作为普通消息发出）。
func (m model) runCommand(text string) (tea.Model, tea.Cmd) {
	m.ta.Reset()
	if strings.HasPrefix(text, "//") {
		msg := text[1:]
		m.emit(transItem{kind: tUser, text: msg})
		return m.submitMessage(msg)
	}
	name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
	args = strings.TrimSpace(args)

	cmds := m.commands()
	if c, ok := lookupCommand(cmds, name); ok {
		return c.run(m, args)
	}

	msg := fmt.Sprintf("未知命令：/%s", name)
	if hint := suggestCommand(cmds, name); hint != "" {
		msg += fmt.Sprintf(" · 是否想用 /%s？", hint)
	}
	m.emit(transItem{kind: tNote, text: msg + " · /help 查看全部命令"})
	return m, nil
}

// runSkill 调用一个技能：懒加载正文、展开参数、作为本拍用户消息发出。
func (m model) runSkill(name, args string) (tea.Model, tea.Cmd) {
	for _, s := range m.skills {
		if s.Name != name {
			continue
		}
		prompt, err := s.Expand(args)
		if err != nil {
			m.emit(transItem{kind: tErr, text: err.Error()})
			return m, nil
		}
		display := "/" + name
		if args != "" {
			display += " " + args
		}
		m.emit(transItem{kind: tUser, text: display})
		m.emit(transItem{kind: tNote, text: "→ 技能：" + name})
		return m.submitMessage(prompt)
	}
	m.emit(transItem{kind: tErr, text: "技能不存在：" + name})
	return m, nil
}

// submitMessage 把一段文本作为用户消息发给 agent（命令路径共用的收尾）。
func (m model) submitMessage(text string) (tea.Model, tea.Cmd) {
	m.state = stateRunning
	m.ta.Blur()
	if m.idle != nil {
		m.idle.Touch()
	}
	return m, m.sendCmd(m.takeShellCtx(text))
}

// suggestCommand 给拼错的命令找最近candidate：前缀命中优先，其次编辑距离 ≤2。
func suggestCommand(cmds []command, input string) string {
	best, bestDist := "", 3
	for _, c := range cmds {
		names := append([]string{c.name}, c.aliases...)
		for _, n := range names {
			if strings.HasPrefix(n, input) && input != "" {
				return n
			}
			if d := editDistance(input, n); d < bestDist {
				best, bestDist = n, d
			}
		}
	}
	return best
}

// editDistance 是经典 Levenshtein（命令名都很短，O(nm) 足够）。
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// filterCommands 是 / 补全菜单的过滤：前缀优先、子串次之（需求 §4.1）。
func filterCommands(cmds []command, prefix string) []command {
	if prefix == "" {
		return cmds
	}
	var pre, sub []command
	for _, c := range cmds {
		if strings.HasPrefix(c.name, prefix) {
			pre = append(pre, c)
		} else if strings.Contains(c.name, prefix) {
			sub = append(sub, c)
		}
	}
	return append(pre, sub...)
}

// ---- 各命令的输出拼装 ----

// helpText 拼 /help 输出：版本/模型/模式头 + 注册表生成的命令列表 + 快捷键。
func (m model) helpText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "TokenCode %s · 模型 %s · 模式 %s\n\n", m.version, m.modelName, m.perms.current().label())
	b.WriteString("命令\n")
	for _, c := range m.commands() {
		if c.source != "" {
			continue
		}
		name := "/" + c.name
		if c.argHint != "" {
			name += " " + c.argHint
		}
		fmt.Fprintf(&b, "  %s %s\n", padCell(name, 28), c.summary)
	}
	if len(m.skills) > 0 {
		b.WriteString("\n技能（/技能名 [参数] 调用）\n")
		for _, s := range m.skills {
			fmt.Fprintf(&b, "  %s %s\n", padCell("/"+s.Name, 28), s.Description)
		}
	}
	if m.mcp != nil {
		b.WriteString("\nMCP：/mcp 查看 server 状态，工具以 mcp__server__tool 自动可用\n")
	}
	b.WriteString("\n快捷键与直通\n")
	b.WriteString("  ⏎ 发送 · Alt+⏎ 换行 · ⇧⇥ 循环权限模式（plan→review→auto→yolo）· ↑↓ 输入历史\n")
	b.WriteString("  ! <命令> 直接跑 shell（输出随下一条消息进上下文）· !! 与 // 转义\n")
	b.WriteString("  PgUp/PgDn/滚轮 滚动 · Ctrl+T 展开/折叠工具执行 · Ctrl+C 打断/退出 · Ctrl+D 退出")
	return b.String()
}

// cmdModel：无参列出可用模型，有参热切换（REQ-2）。
func (m model) cmdModel(args string) (tea.Model, tea.Cmd) {
	if args == "" {
		var b strings.Builder
		fmt.Fprintf(&b, "当前模型：%s\n", m.modelName)
		if len(m.cfg.Models) > 0 {
			b.WriteString("\n别名\n")
			names := make([]string, 0, len(m.cfg.Models))
			for k := range m.cfg.Models {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				mark := "  "
				if m.cfg.Models[k] == m.modelName || strings.HasSuffix(m.cfg.Models[k], "/"+m.modelName) {
					mark = "* "
				}
				fmt.Fprintf(&b, "  %s%-10s → %s\n", mark, k, m.cfg.Models[k])
			}
		}
		if len(m.cfg.Providers) > 0 {
			b.WriteString("\nprovider（/model <provider>/<model-id> 任意切换）\n")
			names := make([]string, 0, len(m.cfg.Providers))
			for k := range m.cfg.Providers {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				fmt.Fprintf(&b, "  %-10s %s（%s）\n", k, m.cfg.Providers[k].BaseURL, m.cfg.Providers[k].Protocol)
			}
		}
		if len(m.cfg.Models) == 0 && len(m.cfg.Providers) == 0 {
			b.WriteString("未配置模型注册表：在 ~/.config/tokencode/config.json 注册 providers/models")
		}
		m.emit(transItem{kind: tNote, text: strings.TrimRight(b.String(), "\n")})
		return m, nil
	}

	if m.switchModel == nil {
		m.emit(transItem{kind: tErr, text: "本会话不支持切换模型"})
		return m, nil
	}
	newModel, newBase, err := m.switchModel(args)
	if err != nil {
		m.emit(transItem{kind: tErr, text: err.Error()})
		return m, nil
	}
	m.modelName, m.baseURL = newModel, newBase
	m.emit(transItem{kind: tNote, text: fmt.Sprintf("→ 模型：%s（%s）", newModel, newBase)})
	m.rebuildContent() // banner 里有模型名，整体重排一次
	return m, nil
}

// agentsText 拼 /agents 输出：可用子代理类型与各自的工具面。
func (m model) agentsText() string {
	if len(m.agentDefs) == 0 {
		return "无子代理可用（agent 工具未装配）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "子代理类型（%d 个）· 模型经 agent 工具委托、workflow 工具编排\n", len(m.agentDefs))
	for _, d := range m.agentDefs {
		toolSet := "全工具"
		if len(d.Tools) > 0 {
			toolSet = strings.Join(d.Tools, ",")
		}
		fmt.Fprintf(&b, "  %s %s\n%s工具：%s · 来源：%s\n",
			padCell(d.Name, 18), d.Description, strings.Repeat(" ", 21), toolSet, d.Source)
	}
	b.WriteString("\n自定义：.tokencode/agents/ 或 .claude/agents/ 下的 *.md（frontmatter: name/description/tools/model，正文=系统提示）")
	return strings.TrimRight(b.String(), "\n")
}

// usageText 拼 /usage 输出：本月合计 + 今天 + 按模型/来源前 5（纯文本表格）。
// WebUI 大盘将来直接吃 usage.Summarize 的结构化结果，这里只是终端排版。
func usageText(now time.Time) string {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	sum, err := usage.Summarize(monthStart, now.Add(time.Second))
	if err != nil {
		return "用量统计不可用：" + err.Error()
	}
	if sum.Total.Calls == 0 {
		return fmt.Sprintf("本月（%s）还没有用量记录（部分端点不报 usage 时无账可记）\n账本目录：%s",
			now.Format("2006-01"), usage.Dir())
	}

	var b strings.Builder
	fmt.Fprintf(&b, "token 用量 · %s\n\n", now.Format("2006-01"))
	b.WriteString(usageHeader("范围"))
	b.WriteString(usageRow("本月", sum.Total))
	b.WriteString(usageRow("今天", sum.ByDay[now.Format("2006-01-02")]))

	b.WriteString("\n按模型（前 5）\n")
	b.WriteString(usageHeader("模型"))
	writeUsageTop(&b, sum.ByModel)

	b.WriteString("\n按来源（前 5）\n")
	b.WriteString(usageHeader("来源"))
	writeUsageTop(&b, sum.BySource)

	fmt.Fprintf(&b, "\n账本：%s（JSONL，按月滚动）", usage.Dir())
	return strings.TrimRight(b.String(), "\n")
}

// usageHeader 是用量表格的表头行（首列名可变）。
func usageHeader(first string) string {
	return "  " + padCell(first, 22) + padNum("调用", 6) + padNum("输入", 10) +
		padNum("输出", 10) + padNum("缓存读", 10) + padNum("缓存写", 10) + "\n"
}

// usageRow 是用量表格的一行：首列左对齐、数字列右对齐。
func usageRow(name string, bk usage.Bucket) string {
	return "  " + padCell(name, 22) + padNum(fmt.Sprintf("%d", bk.Calls), 6) +
		padNum(fmtTokens(bk.In), 10) + padNum(fmtTokens(bk.Out), 10) +
		padNum(fmtTokens(bk.CacheRead), 10) + padNum(fmtTokens(bk.CacheWrite), 10) + "\n"
}

// writeUsageTop 按 in+out 降序取前 5 写表格行。
func writeUsageTop(b *strings.Builder, m map[string]usage.Bucket) {
	type kv struct {
		k string
		v usage.Bucket
	}
	rows := make([]kv, 0, len(m))
	for k, v := range m {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		ti, tj := rows[i].v.In+rows[i].v.Out, rows[j].v.In+rows[j].v.Out
		if ti != tj {
			return ti > tj
		}
		return rows[i].k < rows[j].k
	})
	if len(rows) > 5 {
		rows = rows[:5]
	}
	for _, r := range rows {
		b.WriteString(usageRow(r.k, r.v))
	}
}

// padNum 把 s 左补空格到 w 显示格宽（数字列右对齐；CJK 表头也按格宽算）。
func padNum(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// fmtTokens 把 token 数压成可读形态：1234567 → 1.2M、45678 → 45.7k。
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// skillsText 拼 /skills 输出（REQ-3）。
func (m model) skillsText() string {
	if len(m.skills) == 0 {
		return "未发现技能。技能目录：.tokencode/skills/、.claude/skills/、.agents/skills/（项目级）\n" +
			"或 ~/.config/tokencode/skills/、~/.claude/skills/（用户级），每个技能一个子目录 + SKILL.md"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已发现 %d 个技能（/技能名 [参数] 调用）\n", len(m.skills))
	for _, s := range m.skills {
		desc := s.Description
		if desc == "" {
			desc = "（无描述）"
		}
		fmt.Fprintf(&b, "  /%-12s %s · %s\n", s.Name, desc, s.Source)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cmdMCP：无参列状态；"reconnect <server>" 重连（REQ-4）。
func (m model) cmdMCP(args string) (tea.Model, tea.Cmd) {
	if m.mcp == nil {
		m.emit(transItem{kind: tNote, text: "未配置 MCP server。在 ~/.config/tokencode/config.json 加：\n" +
			`  "mcp": {"名字": {"command": ["npx", "-y", "某个-mcp-server"]}}` + "\n重启后生效，工具以 mcp__名字__工具名 注册"})
		return m, nil
	}
	if sub, rest, _ := strings.Cut(args, " "); sub == "reconnect" {
		name := strings.TrimSpace(rest)
		if name == "" {
			m.emit(transItem{kind: tErr, text: "用法：/mcp reconnect <server>"})
			return m, nil
		}
		if err := m.mcp.Reconnect(name); err != nil {
			m.emit(transItem{kind: tErr, text: err.Error()})
			return m, nil
		}
		m.emit(transItem{kind: tNote, text: "→ 重连中：" + name + "（/mcp 查看进度）"})
		return m, nil
	}

	sts := m.mcp.Statuses()
	sort.Slice(sts, func(i, j int) bool { return sts[i].Name < sts[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "MCP server（%d 个）· /mcp reconnect <server> 重连\n", len(sts))
	for _, st := range sts {
		switch st.State {
		case "ready":
			fmt.Fprintf(&b, "  ● %-12s 已连接 · %d 个工具\n", st.Name, st.Tools)
		case "connecting":
			fmt.Fprintf(&b, "  ◌ %-12s 连接中…\n", st.Name)
		default:
			fmt.Fprintf(&b, "  ✗ %-12s 失败：%s\n", st.Name, st.Err)
		}
	}
	m.emit(transItem{kind: tNote, text: strings.TrimRight(b.String(), "\n")})
	return m, nil
}
