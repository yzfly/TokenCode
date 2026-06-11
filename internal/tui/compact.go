package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
)

// cmdCompact 异步执行 /compact：锁输入、显示 compacting 指示，完成后以
// note 回报压缩前后的条数与估算 tokens。Ctrl+C 沿用打断语义（cancel ctx）。
// 只在 idle 态可达（命令路径），agent 侧的 running CAS 再兜一层底。
func (m model) cmdCompact(args string) (tea.Model, tea.Cmd) {
	if m.agent == nil {
		m.emit(transItem{kind: tErr, text: "本会话不支持压缩"})
		return m, nil
	}
	ag := m.agent
	beforeLen, beforeEst := ag.HistoryLen(), ag.EstimatedTokens()

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.compacting = true
	m.state = stateRunning
	m.ta.Blur()
	m.emit(transItem{kind: tNote, text: "→ 压缩历史上下文…（Ctrl+C 中断）"})
	return m, func() tea.Msg {
		n, err := ag.Compact(ctx, args)
		return compactDoneMsg{
			summarized: n,
			beforeLen:  beforeLen, afterLen: ag.HistoryLen(),
			beforeEst: beforeEst, afterEst: ag.EstimatedTokens(),
			err: err,
		}
	}
}

// contextText 拼 /context 输出：估算与真实 tokens、历史条数与各类占比、
// 距自动压缩阈值的余量。纯文本，排版与 /usage 一致。
func (m model) contextText() string {
	if m.agent == nil {
		return "本会话无上下文统计"
	}
	msgs := m.agent.Snapshot()
	est := agent.EstimateTokens(msgs)

	// 按形态分桶：user 输入（含压缩摘要）、assistant 回复、工具结果回灌。
	var users, assistants, toolResults int
	for _, msg := range msgs {
		switch {
		case msg.Role == llm.RoleAssistant:
			assistants++
		case len(msg.ToolResults) > 0:
			toolResults++
		default:
			users++
		}
	}
	pct := func(n int) string {
		if len(msgs) == 0 {
			return "0%"
		}
		return fmt.Sprintf("%.0f%%", float64(n)*100/float64(len(msgs)))
	}

	var b strings.Builder
	b.WriteString("上下文用量\n\n")
	fmt.Fprintf(&b, "  %s%s tokens（启发式，偏保守）\n", padCell("估算 tokens", 16), fmtTokens(est))
	real := "—（端点未报或还没发过请求）"
	if n := m.agent.LastInputTokens(); n > 0 {
		real = fmtTokens(n) + " tokens"
	}
	fmt.Fprintf(&b, "  %s%s\n", padCell("最近真实输入", 16), real)
	fmt.Fprintf(&b, "  %s%d 条 · user %d（%s）· assistant %d（%s）· 工具结果 %d（%s）\n",
		padCell("历史条数", 16), len(msgs),
		users, pct(users), assistants, pct(assistants), toolResults, pct(toolResults))

	if th := m.agent.AutoCompactThreshold(); th > 0 {
		left := th - est
		if left > 0 {
			fmt.Fprintf(&b, "  %s阈值 %s · 余量 %s tokens（超过即 turn 前自动压缩）",
				padCell("自动压缩", 16), fmtTokens(th), fmtTokens(left))
		} else {
			fmt.Fprintf(&b, "  %s阈值 %s · 已超出 %s tokens，下一拍开始前将自动压缩",
				padCell("自动压缩", 16), fmtTokens(th), fmtTokens(-left))
		}
	} else {
		fmt.Fprintf(&b, "  %s已关闭（config 的 compact.auto_threshold）· /compact 可手动压缩",
			padCell("自动压缩", 16))
	}
	return b.String()
}
