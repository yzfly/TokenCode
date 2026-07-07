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
	"github.com/yzfly/tokencode/internal/checkpoint"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/hooks"
	"github.com/yzfly/tokencode/internal/mcp"
	"github.com/yzfly/tokencode/internal/permrules"
	"github.com/yzfly/tokencode/internal/pulse"
	"github.com/yzfly/tokencode/internal/race"
	"github.com/yzfly/tokencode/internal/skill"
	"github.com/yzfly/tokencode/internal/subagent"
	"github.com/yzfly/tokencode/internal/tools"
)

// Options 是外壳的装配参数。
type Options struct {
	Model   string
	BaseURL string
	Theme   string // auto|light|dark：auto 自动检测终端背景，light/dark 强制
	Yolo    bool
	Notice  string             // 开场提示（如"已恢复会话…"），空=无
	Events  chan agent.Event   // 事件队列：用户输入与心跳共用，由 Serve 顺序消费
	Idle    *pulse.IdleTracker // 用户活动追踪，可为 nil
	Pulse   *pulse.Pulse       // 心跳源，nil=关闭；仅 tty 模式生效

	Cfg         config.Config            // /model 列表
	Hooks       *hooks.Runner            // hooks 运行器，可为 nil；外壳接管 Notify 让提示走 note
	Skills      []skill.Skill            // /skills 与 /技能名
	MCP         *mcp.Manager             // /mcp 状态，可为 nil
	Agents      *subagent.Runner         // 子代理运行器，可为 nil；外壳启动时注入 UI 工厂
	AutoJudge   AutoJudge                // auto 模式权限裁决器，可为 nil
	Rules       *permrules.Rules         // 权限规则三表（全局+项目级已合并），可为 nil
	Workspace   string                   // 工作空间隔离根（显示用）；空=未开启
	Worktree    string                   // -w 的 worktree 名（状态栏显示）；空=未开启
	Checkpoint  *checkpoint.Checkpointer // 文件检查点（/rewind），可为 nil
	SwitchModel func(name string) (model, baseURL string, err error)
	Version     string // /help 头部显示

	// RunRace 驱动一场并行竞赛（/race），可为 nil（命令不可用）。
	// progress 由外壳转投给事件循环，调用方只管按快照回调。
	RunRace func(ctx context.Context, n int, task string, progress func(race.Progress)) (*race.Result, error)
}

// Run 启动外壳。tty 下跑 Bubble Tea；非 tty 退化为纯文本循环
// （plain 模式直接调 ag.Run、不走事件队列，心跳不生效）。
func Run(ag *agent.Agent, opts Options) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return runPlain(ag, opts)
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
	m.agent = ag
	m.cfg, m.skills, m.mcp, m.switchModel = opts.Cfg, opts.Skills, opts.MCP, opts.SwitchModel
	m.version, m.workspace = opts.Version, opts.Workspace
	m.worktree, m.checkpoint = opts.Worktree, opts.Checkpoint
	if opts.Agents != nil {
		m.agentDefs = opts.Agents.Defs()
	}
	if opts.Notice != "" {
		m.transcript = append(m.transcript, transItem{kind: tNote, text: opts.Notice})
		m.rendered = append(m.rendered, m.renderItem(transItem{kind: tNote, text: opts.Notice}))
	}
	// 接管全屏：resize 整屏干净重排；开鼠标以支持滚轮滚动对话。
	// p 先声明后赋值：runRace 闭包要把进度投回事件循环，引用它时已就绪。
	var p *tea.Program
	if opts.RunRace != nil {
		m.runRace = func(ctx context.Context, n int, task string) (*race.Result, error) {
			return opts.RunRace(ctx, n, task, func(pr race.Progress) {
				p.Send(raceProgressMsg{p: pr})
			})
		}
	}
	p = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	// hook 提示（systemMessage/阻断/警告）走 note 上屏（线程安全：prog.Send）。
	if opts.Hooks != nil {
		opts.Hooks.Notify = func(s string) { p.Send(noteMsg{text: "hook · " + s}) }
	}

	br := &bridge{prog: p, perms: perms, judge: opts.AutoJudge, rules: opts.Rules}
	ui := br.UI()

	// 子代理与工作流接同一座桥：权限闸门、转写显示与主 agent 完全同源。
	if opts.Agents != nil {
		opts.Agents.UI = br.SubUI
		opts.Agents.Log = func(text string) { p.Send(noteMsg{text: "wf · " + text}) }
	}

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
func runPlain(ag *agent.Agent, opts Options) error {
	modelName, baseURL, yolo := opts.Model, opts.BaseURL, opts.Yolo
	fmt.Printf("TokenCode · model=%s · base=%s\n", modelName, baseURL)
	if opts.Notice != "" {
		fmt.Println(opts.Notice)
	}
	if opts.Hooks != nil {
		opts.Hooks.Notify = func(s string) { fmt.Println("hook ·", s) }
	}
	// deny 规则全局生效：plain 模式也先查 deny，再走「只读放行，其余看 -yolo」。
	denied := func(name string, input json.RawMessage) bool {
		return opts.Rules.Evaluate(name, input) == permrules.Deny
	}
	// 子代理在 plain 模式下沿用同一条非交互策略。
	if opts.Agents != nil {
		opts.Agents.UI = func(label string) agent.UI {
			return agent.UI{
				OnToolCall: func(name string, input json.RawMessage) bool {
					fmt.Printf("  → [%s] %s %s\n", label, name, oneLine(compactJSON(input), 120))
					return !denied(name, input) && (yolo || tools.ReadOnly(name))
				},
			}
		}
		opts.Agents.Log = func(text string) { fmt.Println("  wf ·", text) }
	}
	streamed := false // 本次完成是否已流式打印过（避免 OnAssistant 重复输出）
	ui := agent.UI{
		OnNote: func(text string) { fmt.Println("·", text) },
		OnAssistantDelta: func(d string) {
			if !streamed {
				fmt.Println()
				streamed = true
			}
			fmt.Print(d)
		},
		OnAssistant: func(t string) {
			if streamed {
				fmt.Println()
				streamed = false
				return
			}
			fmt.Printf("\n%s\n", strings.TrimSpace(t))
		},
		OnToolCall: func(name string, input json.RawMessage) bool {
			fmt.Printf("  → %s %s\n", name, oneLine(compactJSON(input), 120))
			// 非交互：deny 规则先拒，再只读放行，其余除非 -yolo 否则拒绝。
			return !denied(name, input) && (yolo || tools.ReadOnly(name))
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
