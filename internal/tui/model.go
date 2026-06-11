package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/mcp"
	"github.com/yzfly/tokencode/internal/pulse"
	"github.com/yzfly/tokencode/internal/race"
	"github.com/yzfly/tokencode/internal/skill"
	"github.com/yzfly/tokencode/internal/subagent"
)

type uiState int

const (
	stateIdle       uiState = iota // 等用户输入
	stateRunning                   // agent 跑动中
	stateConfirming                // 等工具确认
)

// footerReserve 是底部固定区占的行数：状态/确认行(1) + 输入框(3) + 状态栏(1)。
// viewport 拿走剩下的高度。
const footerReserve = 5

// transKind 标记一条对话项的类型，供按当前宽度重渲染。
type transKind int

const (
	tUser transKind = iota
	tAssistant
	tToolCall
	tToolResult
	tNote
	tErr
	tShell // ! 命令的输出（name=命令，text=输出，result=退出码）
)

// transItem 是一条可重渲染的对话项（存原始数据，不存样式化字符串，
// 这样 resize 时能按新宽度重排）。
type transItem struct {
	kind   transKind
	text   string
	name   string
	agent  string // 子代理标签；空=主 agent
	input  json.RawMessage
	result string
	isErr  bool
}

// model 是 Bubble Tea 根模型。alt-screen 模式：程序接管全屏，对话装在可滚动的
// viewport 里，输入框/状态栏固定底部。resize 时整屏重排，永不残留。
type model struct {
	ta     textarea.Model
	sp     spinner.Model
	vp     viewport.Model
	width  int
	height int
	ready  bool

	state    uiState
	thinking bool
	pending  *confirmReqMsg
	pendingQ []confirmReqMsg // 子代理并行时排队的后续确认请求
	cancel   context.CancelFunc

	history []string
	histIdx int
	draft   string

	events    chan<- agent.Event
	idle      *pulse.IdleTracker // 可为 nil
	perms     *perms
	modelName string
	baseURL   string

	agent       *agent.Agent   // /compact、/context 直接查改历史；可为 nil（测试）
	compacting  bool           // /compact 进行中（输入锁定、指示行显示）
	cfg         config.Config  // /model 列表用
	skills      []skill.Skill  // /skills 列表与 /技能名 调用
	mcp         *mcp.Manager   // /mcp 状态，可为 nil
	agentDefs   []subagent.Def // /agents 列表
	workspace   string         // 工作空间隔离根；空=未开启
	switchModel func(name string) (model, baseURL string, err error)
	version     string

	// / 补全菜单（需求 §4.1）：menuItems 非空即菜单开启；全部来自命令注册表内存数据。
	menuItems    []command
	menuSel      int
	menuSuppress string // Esc 关闭时记下当时的输入值，值不变不重开

	// 竞赛模式（/race）。runRace 由外壳注入（nil=不可用）；racing 期间
	// 输入锁定、状态行显示聚合面板；结果留在 raceResult 等用户 apply/discard。
	runRace    func(ctx context.Context, n int, task string) (*race.Result, error)
	racing     bool
	racePanel  string
	raceResult *race.Result

	transcript    []transItem // 原始对话，用于 resize 重渲染
	rendered      []string    // 各项在当前宽度下的渲染缓存（每条只渲染一次，避免卡）
	toolsExpanded bool        // 是否展开历史工具执行（默认折叠，只留最近 2 个）
	streamBuf     string      // 流式生成中的未完成正文（完成后清空、由最终渲染替换）
	shellCtx      []string    // ! 命令的待注入上下文块，随下一条用户消息发出
	workingOn     string      // 正在执行的工具标签（含子代理前缀）；空=没有工具在跑
}

// visibleToolExecs 是折叠时仍完整显示的最近工具执行数。
const visibleToolExecs = 2

func newModel(events chan<- agent.Event, idle *pulse.IdleTracker, p *perms, modelName, baseURL string) model {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.Placeholder = "输入消息…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	ta.SetHeight(1)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle() // 去掉当前行灰底
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle

	return model{
		ta:        ta,
		sp:        s,
		vp:        viewport.New(0, 0),
		state:     stateIdle,
		events:    events,
		idle:      idle,
		perms:     p,
		modelName: modelName,
		baseURL:   baseURL,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.sp.Tick)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ta.SetWidth(max(20, msg.Width-4))
		m.vp.Width = msg.Width
		m.relayout()
		m.rebuildContent() // 按新宽度整体重排——alt-screen 下整屏干净重绘，永不残留
		m.ready = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case assistantDeltaMsg:
		// 流式期间显示原始文本尾巴；最终的 assistantMsg 会用 markdown 渲染替换它。
		m.streamBuf += msg.text
		m.vp.SetContent(m.viewportContent())
		m.vp.GotoBottom()
		return m, nil

	case assistantMsg:
		m.streamBuf = ""
		m.emit(transItem{kind: tAssistant, text: msg.text})
		return m, nil

	case toolCallMsg:
		m.workingOn = agentTag(msg.agent) + msg.name
		m.emit(transItem{kind: tToolCall, name: msg.name, agent: msg.agent, input: msg.input})
		return m, nil

	case toolResultMsg:
		m.workingOn = ""
		m.emit(transItem{kind: tToolResult, name: msg.name, agent: msg.agent, result: msg.result, isErr: msg.isErr})
		return m, nil

	case noteMsg:
		m.emit(transItem{kind: tNote, text: msg.text})
		return m, nil

	case raceProgressMsg:
		m.racePanel = racePanelText(msg.p)
		return m, nil

	case raceDoneMsg:
		m.racing = false
		m.racePanel = ""
		m.cancel = nil
		m.state = stateIdle
		m.ta.Focus()
		switch {
		case msg.err != nil && errors.Is(msg.err, context.Canceled):
			m.emit(transItem{kind: tNote, text: "（竞赛已中断，worktree 已清理）"})
		case msg.err != nil:
			text := "竞赛失败: " + msg.err.Error()
			if msg.res != nil && len(msg.res.Board) > 0 {
				text += "\n" + raceBoardText(msg.res)
			}
			m.emit(transItem{kind: tErr, text: text})
		default:
			m.raceResult = msg.res
			m.emit(transItem{kind: tNote, text: raceBoardText(msg.res)})
		}
		return m, textarea.Blink

	case compactDoneMsg:
		m.compacting = false
		m.cancel = nil
		m.state = stateIdle
		m.ta.Focus()
		switch {
		case msg.err != nil && errors.Is(msg.err, context.Canceled):
			m.emit(transItem{kind: tNote, text: "（压缩已中断，历史未动）"})
		case msg.err != nil:
			m.emit(transItem{kind: tErr, text: msg.err.Error()})
		case msg.summarized == 0:
			m.emit(transItem{kind: tNote, text: "历史太短，无需压缩"})
		default:
			m.emit(transItem{kind: tNote, text: fmt.Sprintf(
				"已压缩 %d 条历史为摘要 · 条数 %d→%d · 估算 tokens %s→%s",
				msg.summarized, msg.beforeLen, msg.afterLen,
				fmtTokens(msg.beforeEst), fmtTokens(msg.afterEst))})
		}
		return m, textarea.Blink

	case shellDoneMsg:
		m.emit(transItem{kind: tShell, name: msg.cmd, text: msg.out,
			result: fmt.Sprint(msg.exit), isErr: msg.exit != 0})
		m.shellCtx = append(m.shellCtx, shellCtxBlock(msg.cmd, msg.out, msg.exit))
		return m, nil

	case thinkingMsg:
		m.thinking = msg.active
		if msg.active {
			m.workingOn = "" // 回到等模型阶段（被拒的调用没有 result，这里兜底清掉）
		}
		return m, nil

	case runStartedMsg:
		// 只有用户 turn 才接管输入框状态；心跳/梦等后台 turn 不抢焦点、
		// 也不持有 cancel（Ctrl-C 只打断用户自己的 turn）。
		if msg.source != agent.SourceUser {
			return m, nil
		}
		m.cancel = msg.cancel
		m.state = stateRunning
		m.ta.Blur()
		return m, nil

	case confirmReqMsg:
		// 并行子代理可能同时要确认：第一个占住确认框，其余排队。
		if m.pending != nil {
			m.pendingQ = append(m.pendingQ, msg)
			return m, nil
		}
		c := msg
		m.pending = &c
		m.state = stateConfirming
		return m, nil

	case turnDoneMsg:
		if msg.source != agent.SourceUser {
			// 后台 turn 结束：不动输入框状态，错误以普通条目入对话流。
			if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
				m.emit(transItem{kind: tErr, text: msg.err.Error()})
			}
			return m, nil
		}
		m.cancel = nil
		m.thinking = false
		m.workingOn = ""
		m.streamBuf = "" // 中断/出错时丢掉未完成的流式尾巴
		m.state = stateIdle
		m.ta.Focus()
		switch {
		case msg.err == nil:
		case errors.Is(msg.err, context.Canceled):
			m.emit(transItem{kind: tNote, text: "（已中断）"})
		default:
			m.emit(transItem{kind: tErr, text: msg.err.Error()})
		}
		return m, textarea.Blink

	case tea.MouseMsg:
		// 鼠标滚轮滚动对话 viewport（任何状态都能滚）。
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.state == stateIdle {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Shift+Tab 在任何状态都能循环切换权限模式。
	if msg.String() == "shift+tab" {
		nm := m.perms.cycle()
		m.emit(transItem{kind: tNote, text: "→ 模式：" + nm.label()})
		return m, nil
	}

	// Ctrl+T 展开/折叠历史工具执行。
	if msg.String() == "ctrl+t" {
		m.toolsExpanded = !m.toolsExpanded
		m.vp.SetContent(m.viewportContent())
		return m, nil
	}

	switch m.state {
	case stateConfirming:
		choice := choiceReject
		if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
			choice = interpretConfirmKey(byte(msg.Runes[0]))
		}
		if m.pending != nil {
			m.pending.reply <- choice
			m.pending = nil
		}
		// 队列里还有等着的确认（并行子代理）就接着问，否则回到运行态。
		if len(m.pendingQ) > 0 {
			c := m.pendingQ[0]
			m.pendingQ = m.pendingQ[1:]
			m.pending = &c
			return m, nil
		}
		m.state = stateRunning
		return m, nil

	case stateRunning:
		if msg.String() == "ctrl+c" && m.cancel != nil {
			m.cancel()
		}
		return m, nil

	default: // stateIdle
		// 补全菜单开启时，↑↓/Tab/Enter/Esc 先被菜单消费。
		if len(m.menuItems) > 0 {
			switch msg.String() {
			case "up":
				if m.menuSel > 0 {
					m.menuSel--
				}
				return m, nil
			case "down":
				if m.menuSel < len(m.menuItems)-1 {
					m.menuSel++
				}
				return m, nil
			case "tab":
				return m.menuComplete(false)
			case "enter":
				return m.menuComplete(true)
			case "esc":
				m.menuSuppress = m.ta.Value()
				m.menuItems = nil
				m.relayout()
				return m, nil
			}
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "enter":
			return m.submitInput()
		case "pgup":
			m.vp.ViewUp()
			return m, nil
		case "pgdown":
			m.vp.ViewDown()
			return m, nil
		case "up":
			return m.historyPrev()
		case "down":
			return m.historyNext()
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		m.refreshMenu()
		return m, cmd
	}
}

// refreshMenu 按当前输入重算补全菜单：行首 /、未输入空格、非 // 转义时开启。
func (m *model) refreshMenu() {
	v := m.ta.Value()
	open := strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") &&
		!strings.ContainsAny(v, " \n") && v != m.menuSuppress
	if !open {
		m.menuItems = nil
		m.relayout()
		return
	}
	if v != m.menuSuppress {
		m.menuSuppress = ""
	}
	m.menuItems = filterCommands(m.commands(), strings.TrimPrefix(v, "/"))
	if m.menuSel >= len(m.menuItems) {
		m.menuSel = 0
	}
	m.relayout()
}

// menuComplete 接受当前选中项：带参数的命令补全 + 空格等参数；
// 无参数命令在 execute（Enter）时直接执行。
func (m model) menuComplete(execute bool) (tea.Model, tea.Cmd) {
	if len(m.menuItems) == 0 {
		return m, nil
	}
	c := m.menuItems[m.menuSel]
	if c.argHint == "" && execute {
		m.menuItems = nil
		m.relayout()
		m.ta.SetValue("/" + c.name)
		return m.submitInput()
	}
	suffix := ""
	if c.argHint != "" {
		suffix = " "
	}
	m.ta.SetValue("/" + c.name + suffix)
	m.ta.CursorEnd()
	m.refreshMenu()
	return m, nil
}

// relayout 重算 viewport 高度（footer 固定 5 行 + 菜单动态高度）。
func (m *model) relayout() {
	m.vp.Height = max(1, m.height-footerReserve-m.menuHeight())
}

// menuHeight 是菜单当前占的行数（最多 6 行）。
func (m model) menuHeight() int {
	n := len(m.menuItems)
	if n > 6 {
		n = 6
	}
	return n
}

func (m model) submitInput() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return m, nil
	}
	m.history = append(m.history, text)
	m.histIdx = len(m.history)
	m.draft = ""
	m.menuItems = nil
	m.menuSuppress = ""
	m.relayout()
	if strings.HasPrefix(text, "/") {
		return m.runCommand(text)
	}
	// ! shell 直通；!! 转义为普通消息（剥一个 !），与 // 同一惯例。
	if strings.HasPrefix(text, "!") && !strings.HasPrefix(text, "!!") {
		cmd := strings.TrimSpace(text[1:])
		m.ta.Reset()
		if cmd == "" {
			m.emit(transItem{kind: tNote, text: "用法：! <shell 命令>（输出会随下一条消息进入模型上下文）"})
			return m, nil
		}
		m.emit(transItem{kind: tUser, text: text})
		return m, runShell(cmd) // 异步执行，不锁输入
	}
	if strings.HasPrefix(text, "!!") {
		text = text[1:]
	}
	m.ta.Reset()
	m.state = stateRunning
	m.ta.Blur()
	m.emit(transItem{kind: tUser, text: text})
	if m.idle != nil {
		m.idle.Touch()
	}
	return m, m.sendCmd(m.takeShellCtx(text))
}

func (m model) setMode(name string) (tea.Model, tea.Cmd) {
	var mode permMode
	switch name {
	case "plan":
		mode = modePlan
	case "auto":
		mode = modeAuto
	case "yolo":
		mode = modeYolo
	default:
		mode = modeReview
	}
	m.perms.setMode(mode)
	m.ta.Reset()
	m.emit(transItem{kind: tNote, text: "→ 模式：" + mode.label()})
	return m, nil
}

func (m model) sendCmd(text string) tea.Cmd {
	return func() tea.Msg {
		m.events <- agent.Event{Source: agent.SourceUser, Text: text}
		return nil
	}
}

func (m model) historyPrev() (tea.Model, tea.Cmd) {
	if strings.Contains(m.ta.Value(), "\n") || len(m.history) == 0 {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(tea.KeyMsg{Type: tea.KeyUp})
		return m, cmd
	}
	if m.histIdx == len(m.history) {
		m.draft = m.ta.Value()
	}
	if m.histIdx > 0 {
		m.histIdx--
		m.ta.SetValue(m.history[m.histIdx])
		m.ta.CursorEnd()
	}
	m.refreshMenu()
	return m, nil
}

func (m model) historyNext() (tea.Model, tea.Cmd) {
	if strings.Contains(m.ta.Value(), "\n") || m.histIdx >= len(m.history) {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(tea.KeyMsg{Type: tea.KeyDown})
		return m, cmd
	}
	m.histIdx++
	if m.histIdx == len(m.history) {
		m.ta.SetValue(m.draft)
	} else {
		m.ta.SetValue(m.history[m.histIdx])
	}
	m.ta.CursorEnd()
	m.refreshMenu()
	return m, nil
}

// emit 把一条对话项记入 transcript、渲染一次缓存，刷新 viewport 并滚到底。
func (m *model) emit(it transItem) {
	m.transcript = append(m.transcript, it)
	m.rendered = append(m.rendered, m.renderItem(it))
	m.vp.SetContent(m.viewportContent())
	m.vp.GotoBottom()
}

// rebuildContent 按当前宽度把整条对话重渲染一遍（resize 时调用）。
func (m *model) rebuildContent() {
	m.rendered = m.rendered[:0]
	for _, it := range m.transcript {
		m.rendered = append(m.rendered, m.renderItem(it))
	}
	wasBottom := m.vp.AtBottom()
	m.vp.SetContent(m.viewportContent())
	if wasBottom {
		m.vp.GotoBottom()
	}
}

func (m model) viewportContent() string {
	banner := renderBanner(m.modelName, m.baseURL, m.perms.current(), m.width)
	body := m.renderTranscript()
	if m.streamBuf != "" {
		// 流式尾巴：按宽度软换行的原始文本（markdown 渲染等最终消息到了再做一次）。
		tail := lipgloss.NewStyle().Width(m.contentWidth()).Render(m.streamBuf)
		if body == "" {
			body = tail
		} else {
			body += "\n" + tail
		}
	}
	if body == "" {
		return banner
	}
	return banner + "\n\n" + body
}

// renderTranscript 拼出对话正文。默认把历史工具执行折叠成一行（只完整保留
// 最近 visibleToolExecs 个），Ctrl+T 可展开。非工具项（用户/助手/提示）始终显示。
func (m model) renderTranscript() string {
	if m.toolsExpanded {
		return strings.Join(m.rendered, "\n")
	}
	// 统计工具调用，定出"最近 N 个"的起点。
	var calls []int
	for i, it := range m.transcript {
		if it.kind == tToolCall {
			calls = append(calls, i)
		}
	}
	if len(calls) <= visibleToolExecs {
		return strings.Join(m.rendered, "\n")
	}
	visibleFrom := calls[len(calls)-visibleToolExecs] // 此索引及之后的工具项完整显示
	hiddenCount := len(calls) - visibleToolExecs

	parts := make([]string, 0, len(m.rendered))
	summarized := false
	lastCallVisible := true
	for i, it := range m.transcript {
		hidden := false
		switch it.kind {
		case tToolCall:
			lastCallVisible = i >= visibleFrom
			hidden = !lastCallVisible
		case tToolResult:
			hidden = !lastCallVisible
		}
		if hidden {
			if !summarized {
				parts = append(parts, collapseStyle.Render(
					fmt.Sprintf("  ⋯ %d 个历史工具执行已折叠 · Ctrl+T 展开", hiddenCount)))
				summarized = true
			}
			continue
		}
		parts = append(parts, m.rendered[i])
	}
	return strings.Join(parts, "\n")
}

// renderItem 按当前宽度把一条对话项渲染成样式化字符串。
func (m model) renderItem(it transItem) string {
	switch it.kind {
	case tUser:
		return "\n" + userStyle.Render("› "+it.text)
	case tAssistant:
		return renderMarkdown(it.text, m.contentWidth())
	case tToolCall:
		return toolCallStyle.Render("  → "+agentTag(it.agent)+it.name) + " " +
			toolArgStyle.Render(oneLine(compactJSON(it.input), m.previewWidth()))
	case tToolResult:
		mark, st := "✓", okStyle
		if it.isErr {
			mark, st = "✗", errStyle
		}
		return "  " + st.Render(mark) + " " +
			toolArgStyle.Render(agentTag(it.agent)+oneLine(it.result, m.previewWidth()))
	case tErr:
		return errStyle.Render("error: " + it.text)
	case tShell:
		return m.renderShell(it)
	default: // tNote
		return noteStyle.Render(it.text)
	}
}

// renderShell 渲染一条 ! 命令的输出：缩进正文（超长折叠），非零退出标红。
func (m model) renderShell(it transItem) string {
	out := strings.TrimRight(it.text, "\n")
	var b []string
	if out != "" {
		lines := strings.Split(out, "\n")
		if len(lines) > shellShowMax {
			hidden := len(lines) - shellShowMax
			lines = append(lines[:shellShowMax], fmt.Sprintf("…（还有 %d 行，已进上下文）", hidden))
		}
		for _, l := range lines {
			b = append(b, "  "+valueStyle.Render(l))
		}
	}
	if it.isErr {
		b = append(b, "  "+errStyle.Render("✗ exit "+it.result))
	} else if out == "" {
		b = append(b, "  "+noteStyle.Render("（无输出）"))
	}
	return strings.Join(b, "\n")
}

func (m model) View() string {
	if !m.ready {
		return ""
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.vp.View(), m.footerView())
}

// footerView 是底部固定区，正好 footerReserve 行。确认时换成醒目的批准框，
// 否则是「状态/spinner 行 + 输入框 + 状态栏」。
func (m model) footerView() string {
	status := modeBadge(m.perms.current())
	if m.workspace != "" {
		status += " " + planBadge.Render("ws")
	}
	status += " " + hintStyle.Render(m.statusLine())

	var footer string
	if m.state == stateConfirming && m.pending != nil {
		footer = lipgloss.JoinVertical(lipgloss.Left, m.confirmBox(), status)
	} else {
		var top string
		if label := m.statusIndicator(); label != "" {
			top = m.sp.View() + " " + statusStyle.Render(label)
		}
		box := borderFocused
		if m.state != stateIdle {
			box = borderBlurred
		}
		footer = lipgloss.JoinVertical(lipgloss.Left, top, box.Render(m.ta.View()), status)
	}
	if menu := m.menuView(); menu != "" {
		footer = lipgloss.JoinVertical(lipgloss.Left, menu, footer)
	}
	// 截断每行到终端宽度，避免长行折行把布局顶乱。
	return lipgloss.NewStyle().MaxWidth(m.width).Render(footer)
}

// menuView 渲染 / 补全菜单（最多 6 行，选中项超窗时滚动跟随）。
// 命令名蓝、参数提示与摘要灰阶分层；选中行 accent 指示符 + 浅色底铺满整行。
// 列宽按可见项里最宽的左列算，显示格宽计数（CJK 占 2 格）。
func (m model) menuView() string {
	if len(m.menuItems) == 0 {
		return ""
	}
	start := 0
	if m.menuSel >= 6 {
		start = m.menuSel - 5
	}
	end := start + 6
	if end > len(m.menuItems) {
		end = len(m.menuItems)
	}

	labelW := 0
	for i := start; i < end; i++ {
		c := m.menuItems[i]
		if w := lipgloss.Width("/" + c.name + argHintSuffix(c)); w > labelW {
			labelW = w
		}
	}

	lines := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		c := m.menuItems[i]
		name, hint := "/"+c.name, argHintSuffix(c)
		gap := strings.Repeat(" ", labelW-lipgloss.Width(name+hint)+2)
		summary := c.summary
		if c.source != "" {
			summary += " · 技能"
		}
		var line string
		if i == m.menuSel {
			line = menuCursorStyle.Render(" ❯ ") + menuNameSelStyle.Render(name) +
				menuHintSelStyle.Render(hint) + menuSelFill.Render(gap) + menuSumSelStyle.Render(summary)
			if pad := m.width - lipgloss.Width(line); pad > 0 {
				line += menuSelFill.Render(strings.Repeat(" ", pad))
			}
		} else {
			line = "   " + menuNameStyle.Render(name) + menuHintStyle.Render(hint) +
				gap + menuSumStyle.Render(summary)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// argHintSuffix 是命令左列里参数提示的部分（带前导空格，无参数则为空）。
func argHintSuffix(c command) string {
	if c.argHint == "" {
		return ""
	}
	return " " + c.argHint
}

// padCell 把 s 右补空格到 w 显示格宽。fmt 的 %-Ns 按字节数补齐，
// 中文（2 格宽、3 字节）一掺进来列就歪，这里按终端实际格宽算。
func padCell(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// agentTag 是工具行的子代理标签前缀（空标签返回空串）。
func agentTag(label string) string {
	if label == "" {
		return ""
	}
	return "[" + label + "] "
}

// confirmBox 是醒目的工具批准框（4 行：边框 + 2 内容行），替代输入框位置。
func (m model) confirmBox() string {
	innerW := max(20, m.width-6)
	name := agentTag(m.pending.agent) + m.pending.name
	args := oneLine(compactJSON(m.pending.input), max(8, innerW-len(name)-12))
	line1 := confirmBadge.Render(" 需要批准 ") + " " +
		toolCallStyle.Bold(true).Render(name) + " " + toolArgStyle.Render(args)
	line2 := keyYes.Render("[y] 执行") + "   " + keyNo.Render("[n] 拒绝") + "   " +
		keyAll.Render("[a] 本会话一直允许")
	return confirmBoxStyle.Width(innerW).Render(line1 + "\n" + line2)
}

// statusIndicator 返回 turn 进行中的阶段指示：等模型是 thinking，
// 执行工具是 working（带当前工具名，含子代理标签）。空串=无事发生。
// 后台 turn（心跳/梦）只在等模型时给指示，与原行为一致。
func (m model) statusIndicator() string {
	switch {
	case m.racing:
		return m.racePanel
	case m.compacting:
		return "compacting · 正在压缩历史上下文…"
	case m.thinking:
		return "thinking…"
	case m.state != stateRunning:
		return ""
	case m.workingOn != "":
		return "working · " + m.workingOn + "…"
	default:
		return "working…"
	}
}

func (m model) statusLine() string {
	return m.modelName + " · ⏎ 发送 · ⇧⇥ 模式 · ↑↓ 历史 · /help · ^C " +
		map[bool]string{true: "打断", false: "退出"}[m.state == stateRunning]
}

// contentWidth 是 markdown 渲染可用的宽度。
func (m model) contentWidth() int {
	if m.width <= 2 {
		return 80
	}
	return m.width - 2
}

// previewWidth 是工具调用/结果单行预览的最大 rune 数。
func (m model) previewWidth() int {
	w := m.contentWidth() - 6
	if w < 20 {
		w = 20
	}
	if w > 140 {
		w = 140
	}
	return w
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
