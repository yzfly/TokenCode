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
	"github.com/yzfly/tokencode/internal/pulse"
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
)

// transItem 是一条可重渲染的对话项（存原始数据，不存样式化字符串，
// 这样 resize 时能按新宽度重排）。
type transItem struct {
	kind   transKind
	text   string
	name   string
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
	cancel   context.CancelFunc

	history []string
	histIdx int
	draft   string

	events    chan<- agent.Event
	idle      *pulse.IdleTracker // 可为 nil
	perms     *perms
	modelName string
	baseURL   string

	transcript    []transItem // 原始对话，用于 resize 重渲染
	rendered      []string    // 各项在当前宽度下的渲染缓存（每条只渲染一次，避免卡）
	toolsExpanded bool        // 是否展开历史工具执行（默认折叠，只留最近 2 个）
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
		m.vp.Height = max(1, msg.Height-footerReserve)
		m.rebuildContent() // 按新宽度整体重排——alt-screen 下整屏干净重绘，永不残留
		m.ready = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case assistantMsg:
		m.emit(transItem{kind: tAssistant, text: msg.text})
		return m, nil

	case toolCallMsg:
		m.emit(transItem{kind: tToolCall, name: msg.name, input: msg.input})
		return m, nil

	case toolResultMsg:
		m.emit(transItem{kind: tToolResult, name: msg.name, result: msg.result, isErr: msg.isErr})
		return m, nil

	case thinkingMsg:
		m.thinking = msg.active
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
		m.state = stateRunning
		return m, nil

	case stateRunning:
		if msg.String() == "ctrl+c" && m.cancel != nil {
			m.cancel()
		}
		return m, nil

	default: // stateIdle
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
		return m, cmd
	}
}

func (m model) submitInput() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.ta.Value())
	if text == "" {
		return m, nil
	}
	switch text {
	case "/exit", "/quit":
		return m, tea.Quit
	case "/plan", "/review", "/yolo":
		return m.setMode(text[1:])
	}
	m.history = append(m.history, text)
	m.histIdx = len(m.history)
	m.draft = ""
	m.ta.Reset()
	m.state = stateRunning
	m.ta.Blur()
	m.emit(transItem{kind: tUser, text: text})
	if m.idle != nil {
		m.idle.Touch()
	}
	return m, m.sendCmd(text)
}

func (m model) setMode(name string) (tea.Model, tea.Cmd) {
	var mode permMode
	switch name {
	case "plan":
		mode = modePlan
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
		return toolCallStyle.Render("  → "+it.name) + " " +
			toolArgStyle.Render(oneLine(compactJSON(it.input), m.previewWidth()))
	case tToolResult:
		mark, st := "✓", okStyle
		if it.isErr {
			mark, st = "✗", errStyle
		}
		return "  " + st.Render(mark) + " " + toolArgStyle.Render(oneLine(it.result, m.previewWidth()))
	case tErr:
		return errStyle.Render("error: " + it.text)
	default: // tNote
		return noteStyle.Render(it.text)
	}
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
	status := modeBadge(m.perms.current()) + " " + hintStyle.Render(m.statusLine())

	var footer string
	if m.state == stateConfirming && m.pending != nil {
		footer = lipgloss.JoinVertical(lipgloss.Left, m.confirmBox(), status)
	} else {
		var top string
		if m.thinking {
			top = m.sp.View() + " " + statusStyle.Render("thinking…")
		}
		box := borderFocused
		if m.state != stateIdle {
			box = borderBlurred
		}
		footer = lipgloss.JoinVertical(lipgloss.Left, top, box.Render(m.ta.View()), status)
	}
	// 截断每行到终端宽度，避免长行折行把布局顶乱。
	return lipgloss.NewStyle().MaxWidth(m.width).Render(footer)
}

// confirmBox 是醒目的工具批准框（4 行：边框 + 2 内容行），替代输入框位置。
func (m model) confirmBox() string {
	innerW := max(20, m.width-6)
	args := oneLine(compactJSON(m.pending.input), max(8, innerW-len(m.pending.name)-12))
	line1 := confirmBadge.Render(" 需要批准 ") + " " +
		toolCallStyle.Bold(true).Render(m.pending.name) + " " + toolArgStyle.Render(args)
	line2 := keyYes.Render("[y] 执行") + "   " + keyNo.Render("[n] 拒绝") + "   " +
		keyAll.Render("[a] 本会话一直允许")
	return confirmBoxStyle.Width(innerW).Render(line1 + "\n" + line2)
}

func (m model) statusLine() string {
	return m.modelName + " · ⏎ 发送 · ⇧⇥ 模式 · ↑↓ 历史 · PgUp/PgDn 滚动 · ^C " +
		map[bool]string{true: "打断", false: "退出"}[m.state == stateRunning] +
		" · /exit"
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
