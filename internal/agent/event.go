package agent

import (
	"context"
	"strings"

	"github.com/yzfly/tokencode/internal/llm"
)

// EventSource 标识一拍（一个 turn）的发起者。
type EventSource string

const (
	SourceUser      EventSource = "user"      // 用户输入（交互式，允许阻塞确认）
	SourceHeartbeat EventSource = "heartbeat" // 周期心跳
	SourceDream     EventSource = "dream"     // 梦醒通知
)

// Sentinel 是空转哨兵：Ephemeral 事件的最终回复等于它时，
// 这一拍视为无事发生，从历史中剔除（不污染上下文）。
const Sentinel = "HEARTBEAT_OK"

// MemoryPath 是长期记忆文件的相对路径：做梦重写它，SystemPrompt 注入它。
const MemoryPath = ".tokencode/memory.md"

// Event 是投给 agent actor 的一拍：用户消息、心跳、梦醒通知共用同一原语。
type Event struct {
	Source    EventSource
	Text      string
	Ephemeral bool // true 时空转结果（纯文本哨兵回复）不留进历史
}

// Serve 让 agent 成为顺序消费事件的 actor：所有 turn 串行执行。
// 并发不变量：a.msgs 只由这个 goroutine 写。ctx 取消或 events 关闭时返回。
func (a *Agent) Serve(ctx context.Context, events <-chan Event, ui UI) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			a.serveOne(ctx, ev, ui)
		}
	}
}

// Busy 报告是否有 turn 正在执行（任意来源）。仅对 Serve 模式有意义，
// 心跳用它跳过「用户正在干活」的拍。
func (a *Agent) Busy() bool {
	return a.running.Load()
}

// serveOne 执行一拍：造 per-turn ctx（cancel 经 OnTurnStart 交给外壳，
// 沿用既有的打断语义），跑 turn，再按 Ephemeral 语义清理历史。
func (a *Agent) serveOne(ctx context.Context, ev Event, ui UI) {
	a.running.Store(true)
	defer a.running.Store(false)

	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if ui.OnTurnStart != nil {
		ui.OnTurnStart(ev.Source, cancel)
	}

	before := a.HistoryLen()
	turnUI := ui
	if ev.Ephemeral {
		// 空转哨兵不上屏：只拦显示，历史的剔除在 turn 结束后做。
		inner := ui.OnAssistant
		turnUI.OnAssistant = func(text string) {
			if strings.TrimSpace(text) == Sentinel {
				return
			}
			if inner != nil {
				inner(text)
			}
		}
	}

	err := a.runTurn(tctx, ev.Text, turnUI)
	if ev.Ephemeral {
		if err != nil {
			// 失败的后台拍不留痕，避免悬空的 user 消息破坏下一次请求。
			a.truncate(before)
		} else {
			a.dropIfIdleTurn(before)
		}
	}

	if ui.OnTurnDone != nil {
		ui.OnTurnDone(ev.Source, err)
	}
}

// dropIfIdleTurn 检查刚结束的一拍是否空转——恰好新增 user+assistant 两条、
// assistant 无工具调用且纯文本等于哨兵——是则整拍剔除。
func (a *Agent) dropIfIdleTurn(before int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.msgs) != before+2 {
		return
	}
	last := a.msgs[len(a.msgs)-1]
	if last.Role != llm.RoleAssistant || len(last.ToolUses) != 0 {
		return
	}
	if strings.TrimSpace(last.Text) != Sentinel {
		return
	}
	a.msgs = a.msgs[:before]
}
