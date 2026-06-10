package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yzfly/tokencode/internal/race"
)

// 竞赛模式的 TUI 侧：/race 命令解析、聚合进度面板、排行榜与确认应用。
// 竞赛本体在 internal/race，外壳经 Options.RunRace 注入的闭包驱动它。

type (
	raceProgressMsg struct{ p race.Progress }
	raceDoneMsg     struct {
		res *race.Result
		err error
	}
)

// cmdRace 分发 /race：
//
//	/race <N> <任务>   开一场竞赛
//	/race apply        应用上一场冠军的改动
//	/race discard      放弃上一场结果（冠军分支保留）
func (m model) cmdRace(args string) (tea.Model, tea.Cmd) {
	if m.runRace == nil {
		m.emit(transItem{kind: tErr, text: "本会话不支持竞赛模式"})
		return m, nil
	}
	first, rest, _ := strings.Cut(args, " ")
	switch first {
	case "apply":
		return m.raceApply()
	case "discard":
		return m.raceDiscard()
	}

	n, err := strconv.Atoi(first)
	task := strings.TrimSpace(rest)
	if err != nil || task == "" {
		m.emit(transItem{kind: tNote, text: "用法：/race <N> <任务描述>（N=1-" + strconv.Itoa(race.MaxN) + "）· /race apply 应用冠军 · /race discard 放弃"})
		return m, nil
	}
	if n < 1 || n > race.MaxN {
		m.emit(transItem{kind: tErr, text: fmt.Sprintf("N 必须在 1-%d 之间", race.MaxN)})
		return m, nil
	}

	m.emit(transItem{kind: tUser, text: "/race " + args})
	m.emit(transItem{kind: tNote, text: fmt.Sprintf(
		"⚑ 竞赛开始：%d 个 agent 各自在隔离 worktree 里独立解题，裁判择优。\n"+
			"  racer 在各自写空间内自动放行全部工具（含 bash）· Ctrl+C 中止", n)})

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state = stateRunning
	m.racing = true
	m.racePanel = fmt.Sprintf("race · %d 排队 (N=%d)", n, n)
	m.ta.Blur()
	if m.idle != nil {
		m.idle.Touch()
	}
	run := m.runRace
	return m, func() tea.Msg {
		res, err := run(ctx, n, task)
		return raceDoneMsg{res: res, err: err}
	}
}

// raceApply 把上一场冠军 diff 应用到主工作区。
func (m model) raceApply() (tea.Model, tea.Cmd) {
	res := m.raceResult
	if res == nil || res.Winner == nil {
		m.emit(transItem{kind: tNote, text: "没有待应用的竞赛结果（先 /race <N> <任务>）"})
		return m, nil
	}
	if err := race.Apply(context.Background(), res.RepoRoot, res.Winner.Diff); err != nil {
		m.emit(transItem{kind: tErr, text: err.Error() + "\n冠军分支 " + res.Winner.Branch + " 仍保留，可手动 merge"})
		return m, nil
	}
	m.emit(transItem{kind: tNote, text: fmt.Sprintf(
		"✓ 已应用冠军 #%d 的改动到主工作区（未提交，请审查后自行 commit）\n  分支 %s 保留可追溯",
		res.Winner.Index, res.Winner.Branch)})
	m.raceResult = nil
	return m, nil
}

func (m model) raceDiscard() (tea.Model, tea.Cmd) {
	res := m.raceResult
	if res == nil {
		m.emit(transItem{kind: tNote, text: "没有待处理的竞赛结果"})
		return m, nil
	}
	note := "已放弃竞赛结果"
	if res.Winner != nil {
		note += "（冠军分支 " + res.Winner.Branch + " 保留，可手动 cherry-pick/merge）"
	}
	m.emit(transItem{kind: tNote, text: note})
	m.raceResult = nil
	return m, nil
}

// racePanelText 把进度快照拼成一行面板。
func racePanelText(p race.Progress) string {
	switch p.Phase {
	case "judging":
		return fmt.Sprintf("race · 裁判打分 %d/%d", p.Scored, p.Judging)
	case "final":
		return "race · 决赛终审中"
	default:
		return fmt.Sprintf("race · %d 运行 · %d 排队 · %d 完成 · %d 出局 (N=%d)",
			p.Running, p.Queued, p.Done, p.Failed, p.N)
	}
}

// raceBoardText 渲染排行榜与冠军详情。
func raceBoardText(res *race.Result) string {
	var b strings.Builder
	if res.Winner != nil {
		fmt.Fprintf(&b, "🏆 竞赛结束 · 冠军 #%d（%s）\n", res.Winner.Index, res.Winner.Branch)
		fmt.Fprintf(&b, "   终审：%s\n", res.Reason)
		if res.Winner.DiffStat != "" {
			fmt.Fprintf(&b, "   改动：%s\n", res.Winner.DiffStat)
		}
	} else {
		b.WriteString("竞赛结束 · 无冠军\n")
	}
	b.WriteString("\n排行榜\n")
	for _, c := range res.Board {
		switch {
		case c.Out != "":
			fmt.Fprintf(&b, "  ✗ #%-3d 出局 · %s\n", c.Index, c.Out)
		case res.Winner != nil && c.Index == res.Winner.Index:
			fmt.Fprintf(&b, "  🏆 #%-3d %s\n", c.Index, oneLine(c.Reason, 90))
		default:
			fmt.Fprintf(&b, "  · #%-3d %d 分 · %s\n", c.Index, c.Score, oneLine(c.Reason, 80))
		}
	}
	if res.Winner != nil {
		b.WriteString("\n→ /race apply 应用冠军改动 · /race discard 放弃（分支保留）")
	}
	return strings.TrimRight(b.String(), "\n")
}
