package tui

import (
	"context"
	"encoding/json"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/permrules"
	"github.com/yzfly/tokencode/internal/tools"
)

// 在 worker goroutine 与 Bubble Tea 事件循环之间传递的消息类型。
// worker 只发原始数据，所有渲染在 Update 里单线程完成。
// agent 字段是子代理标签（空=主 agent）。
type (
	assistantMsg      struct{ text string }
	assistantDeltaMsg struct{ text string }
	noteMsg           struct{ text string }
	toolCallMsg       struct {
		agent string
		name  string
		input json.RawMessage
	}
	toolResultMsg struct {
		agent  string
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

	// compactDoneMsg 是 /compact 异步完成的回报：压缩前后的条数与估算 tokens。
	compactDoneMsg struct {
		summarized          int // 被折叠进摘要的消息条数（0=历史太短没压）
		beforeLen, afterLen int
		beforeEst, afterEst int
		err                 error
	}

	// confirmReqMsg 由 worker 发出后阻塞等 reply；Model 显示确认、收键后写回。
	// 子代理并行时可能同时有多个在途，Model 排队逐个确认。
	confirmReqMsg struct {
		agent string
		name  string
		input json.RawMessage
		reply chan confirmChoice
	}
)

// AutoJudge 是 auto 模式的权限裁决器：根据规则状态判定一次工具调用，
// 返回是否放行与一句理由。err 非 nil 时落回人工确认。
type AutoJudge func(name string, input json.RawMessage) (allow bool, reason string, err error)

// bridge 持有 program 引用与共享权限状态，构造 agent 所需的 UI 回调。
type bridge struct {
	prog  *tea.Program
	perms *perms
	judge AutoJudge        // auto 模式裁决器，可为 nil（nil 时 auto 退化为人工确认）
	rules *permrules.Rules // 权限规则三表，可为 nil（恒 NoMatch）
	// src 是当前 turn 的来源。所有回调都来自 Serve 的 actor goroutine
	// 且 OnTurnStart 先于该 turn 的其它回调，因此普通字段即安全。
	src agent.EventSource
}

// UI 把主 agent 的回调全部转成 program.Send。
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
		OnToolCall:   func(name string, input json.RawMessage) bool { return b.gateTool("", name, input) },
		OnToolResult: func(name, result string, isErr bool) { b.prog.Send(toolResultMsg{"", name, result, isErr}) },
		OnThinking:   func(active bool) { b.prog.Send(thinkingMsg{active}) },
		OnNote:       func(text string) { b.prog.Send(noteMsg{text: text}) },
	}
}

// SubUI 构造子代理的 UI 回调：工具调用走与主 agent 同一套权限闸门，
// 显示带子代理标签；文本/spinner 不上屏（最终文本作为工具结果返回主 agent）。
func (b *bridge) SubUI(label string) agent.UI {
	return agent.UI{
		OnToolCall: func(name string, input json.RawMessage) bool { return b.gateTool(label, name, input) },
		OnToolResult: func(name, result string, isErr bool) {
			b.prog.Send(toolResultMsg{label, name, result, isErr})
		},
	}
}

// gateTool 是工具调用的权限闸门（主 agent 与子代理共用）。
// 裁决链：权限规则三表（deny 直接拒、allow 直接放行、ask 强制人工确认）
// → 模式默认。非交互来源（心跳/梦）只许只读且 deny 全局生效；
// 需要模式确认时：auto 模式先问小模型，失败或非 auto 走阻塞式人工确认。
// 子代理并行时多个确认请求各自带 reply channel，Model 排队逐个处理。
func (b *bridge) gateTool(label, name string, input json.RawMessage) bool {
	b.prog.Send(toolCallMsg{label, name, input})
	rd := b.rules.Evaluate(name, input)
	if b.src != agent.SourceUser {
		return tools.ReadOnly(name) && rd != permrules.Deny // v1 从严：非交互 turn 只许只读
	}
	switch resolveGate(rd, b.perms.decide(name)) {
	case gateAllow:
		return true
	case gateReject:
		if rd == permrules.Deny {
			b.prog.Send(noteMsg{text: "✗ 拒绝 " + name + " · 命中权限规则 deny"})
		}
		return false
	case gateConfirmMode:
		// auto 模式先问小模型；失败落回人工确认。
		if b.perms.current() == modeAuto && b.judge != nil {
			allow, reason, err := b.judge(name, input)
			if err == nil {
				mark := "✗ 拒绝"
				if allow {
					mark = "✓ 放行"
				}
				b.prog.Send(noteMsg{text: fmt.Sprintf("auto %s %s · %s", mark, name, reason)})
				return allow
			}
			b.prog.Send(noteMsg{text: "auto 裁决失败（" + err.Error() + "），转人工确认"})
		}
	case gateConfirmHuman:
		b.prog.Send(noteMsg{text: "命中权限规则 ask：" + name + " 需人工确认"})
	}

	reply := make(chan confirmChoice, 1)
	b.prog.Send(confirmReqMsg{agent: label, name: name, input: input, reply: reply})
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
