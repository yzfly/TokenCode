package tui

import (
	"context"
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yzfly/tokencode/internal/agent"
)

// 在 worker goroutine 与 Bubble Tea 事件循环之间传递的消息类型。
// worker 只发原始数据，所有渲染在 Update 里单线程完成。
type (
	assistantMsg      struct{ text string }
	assistantDeltaMsg struct{ text string }
	toolCallMsg       struct {
		name  string
		input json.RawMessage
	}
	toolResultMsg struct {
		name   string
		result string
		isErr  bool
	}
	thinkingMsg   struct{ active bool }
	runStartedMsg struct {
		source agent.EventSource
		cancel context.CancelFunc
	}
	turnDoneMsg struct {
		source agent.EventSource
		err    error
	}

	// confirmReqMsg 由 worker 发出后阻塞等 reply；Model 显示确认、收键后写回。
	confirmReqMsg struct {
		name  string
		input json.RawMessage
		reply chan confirmChoice
	}
)

// bridge 持有 program 引用与共享权限状态，构造 agent 所需的 UI 回调。
type bridge struct {
	prog  *tea.Program
	perms *perms
	// src 是当前 turn 的来源。所有回调都来自 Serve 的 actor goroutine
	// 且 OnTurnStart 先于该 turn 的其它回调，因此普通字段即安全。
	src agent.EventSource
}

// UI 把 agent 的回调全部转成 program.Send。
// 工具确认只对用户来源走阻塞 channel 会合；心跳/梦等非交互来源没人按键，
// 走自动策略（只读放行、写类一律拒绝），绝不阻塞在 reply 上。
func (b *bridge) UI() agent.UI {
	return agent.UI{
		OnTurnStart: func(source agent.EventSource, cancel context.CancelFunc) {
			b.src = source
			b.prog.Send(runStartedMsg{source: source, cancel: cancel})
		},
		OnTurnDone: func(source agent.EventSource, err error) {
			b.prog.Send(turnDoneMsg{source: source, err: err})
		},
		OnAssistant: func(text string) { b.prog.Send(assistantMsg{text}) },
		OnAssistantDelta: func(text string) {
			// 只有用户 turn 流式上屏；后台 turn 的结果走完整的 OnAssistant。
			if b.src == agent.SourceUser {
				b.prog.Send(assistantDeltaMsg{text})
			}
		},
		OnToolCall: func(name string, input json.RawMessage) bool {
			b.prog.Send(toolCallMsg{name, input})
			if b.src != agent.SourceUser {
				return name == "read" // v1 从严：非交互 turn 只许只读
			}
			switch b.perms.decide(name) {
			case permAllow:
				return true
			case permReject:
				return false
			default: // permConfirm
				reply := make(chan confirmChoice, 1)
				b.prog.Send(confirmReqMsg{name: name, input: input, reply: reply})
				switch <-reply {
				case choiceAllowAlways:
					b.perms.rememberAlways(name)
					return true
				case choiceAllowOnce:
					return true
				default:
					return false
				}
			}
		},
		OnToolResult: func(name, result string, isErr bool) {
			b.prog.Send(toolResultMsg{name, result, isErr})
		},
		OnThinking: func(active bool) { b.prog.Send(thinkingMsg{active}) },
	}
}
