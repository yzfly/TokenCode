package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/pulse"
)

// Options 是外壳的装配参数。
type Options struct {
	Model   string
	BaseURL string
	Theme   string // auto|light|dark：auto 自动检测终端背景，light/dark 强制
	Yolo    bool
	Events  chan agent.Event   // 事件队列：用户输入与心跳共用，由 Serve 顺序消费
	Idle    *pulse.IdleTracker // 用户活动追踪，可为 nil
	Pulse   *pulse.Pulse       // 心跳源，nil=关闭；仅 tty 模式生效
}

// Run 启动外壳。tty 下跑 Bubble Tea；非 tty 退化为纯文本循环
// （plain 模式直接调 ag.Run、不走事件队列，心跳不生效）。
func Run(ag *agent.Agent, opts Options) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return runPlain(ag, opts.Model, opts.BaseURL, opts.Yolo)
	}
	theme, yolo := opts.Theme, opts.Yolo

	// 决定明暗：auto 检测终端背景（要在进 raw 模式前查询），否则按指定强制。
	// 锁定给 lipgloss 的 AdaptiveColor 与 glamour，使配色在亮/暗底都清晰。
	switch theme {
	case "light":
		uiDark = false
	case "dark":
		uiDark = true
	default:
		uiDark = lipgloss.HasDarkBackground()
	}
	lipgloss.SetHasDarkBackground(uiDark)

	// -yolo 决定初始模式；运行时可用 Shift+Tab / slash 命令切换。
	initial := modeReview
	if yolo {
		initial = modeYolo
	}
	perms := newPerms(initial)

	m := newModel(opts.Events, opts.Idle, perms, opts.Model, opts.BaseURL)
	// 接管全屏：resize 整屏干净重排；开鼠标以支持滚轮滚动对话。
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	br := &bridge{prog: p, perms: perms}
	ui := br.UI()

	// actor：Serve 顺序消费事件队列（用户输入 + 心跳），所有回调经 program.Send 投递。
	// per-turn 的 cancel 由 Serve 造、经 runStartedMsg 交给 model（打断语义不变）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Serve(ctx, opts.Events, ui)
	if opts.Pulse != nil {
		go opts.Pulse.Start(ctx)
	}

	_, err := p.Run()
	return err
}

// runPlain 是非 tty（管道/重定向）下的极简纯文本回退，不进 Bubble Tea。
func runPlain(ag *agent.Agent, modelName, baseURL string, yolo bool) error {
	fmt.Printf("TokenCode · model=%s · base=%s\n", modelName, baseURL)
	ui := agent.UI{
		OnAssistant: func(t string) { fmt.Printf("\n%s\n", strings.TrimSpace(t)) },
		OnToolCall: func(name string, input json.RawMessage) bool {
			fmt.Printf("  → %s %s\n", name, oneLine(compactJSON(input), 120))
			return yolo || name == "read" // 非交互：只读放行，其余除非 -yolo 否则拒绝
		},
		OnToolResult: func(name, result string, isErr bool) {
			mark := "✓"
			if isErr {
				mark = "✗"
			}
			fmt.Printf("  %s %s\n", mark, oneLine(result, 200))
		},
	}
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\n> ")
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Println()
			return nil
		}
		msg := strings.TrimSpace(line)
		if msg == "" {
			continue
		}
		if msg == "/exit" || msg == "/quit" {
			return nil
		}
		if err := ag.Run(context.Background(), msg, ui); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}
