// Package agent 实现单 agent 的 tool-use 循环——TokenCode 的核心底座单元。
// 后续的并行运行时将建立在多个这样的 agent 之上。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// UI 是 agent 与外界交互的回调（打印 / 确认）。各回调可为 nil。
type UI struct {
	// OnAssistant 在模型产出文本时调用。
	OnAssistant func(text string)
	// OnToolCall 在执行工具前调用，返回 false 表示拒绝该次调用。
	OnToolCall func(name string, input json.RawMessage) bool
	// OnToolResult 在工具执行后调用。
	OnToolResult func(name, result string, isErr bool)
	// OnThinking 在每次调用模型前后触发（true=开始等待，false=结束）。
	// 供外壳显示/隐藏 spinner。可为 nil。
	OnThinking func(active bool)
	// OnTurnStart 在 Serve 的每拍开始时调用，携带来源与该拍的取消函数。
	// 外壳据此决定是否切换输入状态、持有 cancel 以支持打断。仅 Serve 路径触发。
	OnTurnStart func(source EventSource, cancel context.CancelFunc)
	// OnTurnDone 在 Serve 的每拍结束时调用。仅 Serve 路径触发。
	OnTurnDone func(source EventSource, err error)
}

// Agent 持有对话状态，驱动 tool-use 循环。
// 并发不变量：msgs 只被一个 turn 执行者（Run 调用方或 Serve actor）追加/截断，
// mu 只为让 Snapshot 能从其它 goroutine 安全读；已入历史的元素从不改写。
type Agent struct {
	llm       llm.LLM
	tools     *tools.Registry
	model     string
	maxTokens int

	mu      sync.Mutex
	msgs    []llm.Message
	running atomic.Bool // 是否有 turn 正在执行（仅 Serve 路径维护）
}

// New 创建一个 agent。
func New(client llm.LLM, reg *tools.Registry, model string, maxTokens int) *Agent {
	return &Agent{
		llm:       client,
		tools:     reg,
		model:     model,
		maxTokens: maxTokens,
	}
}

// Run 接收一句用户输入，跑完一轮 tool-use 循环（可能多次调用模型）。
func (a *Agent) Run(ctx context.Context, userInput string, ui UI) error {
	return a.runTurn(ctx, userInput, ui)
}

// Snapshot 返回对话历史的值拷贝。元素内部的 slice 与原历史共享底层数组，
// 但 agent 只追加、从不改写已有元素，读方只读即安全（dreamer 用）。
func (a *Agent) Snapshot() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]llm.Message, len(a.msgs))
	copy(out, a.msgs)
	return out
}

// HistoryLen 返回当前历史条数（做梦判定的零成本输入）。
func (a *Agent) HistoryLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.msgs)
}

func (a *Agent) append(ms ...llm.Message) {
	a.mu.Lock()
	a.msgs = append(a.msgs, ms...)
	a.mu.Unlock()
}

func (a *Agent) truncate(n int) {
	a.mu.Lock()
	if n >= 0 && n <= len(a.msgs) {
		a.msgs = a.msgs[:n]
	}
	a.mu.Unlock()
}

// history 取当前历史的 slice 头（供本 turn 的请求用；turn 进行中无人追加）。
func (a *Agent) history() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.msgs
}

// runTurn 是一拍的主体，Run 与 Serve 共用。
// system prompt 每拍重建：memory.md 被梦重写后，下个 turn 自然生效。
func (a *Agent) runTurn(ctx context.Context, userInput string, ui UI) error {
	system := SystemPrompt()
	a.append(llm.Message{Role: llm.RoleUser, Text: userInput})

	for {
		if ui.OnThinking != nil {
			ui.OnThinking(true)
		}
		resp, err := a.llm.Complete(ctx, llm.Request{
			Model:     a.model,
			System:    system,
			Messages:  a.history(),
			Tools:     a.toolDefs(),
			MaxTokens: a.maxTokens,
		})
		if ui.OnThinking != nil {
			ui.OnThinking(false)
		}
		if err != nil {
			return err
		}

		a.append(llm.Message{
			Role:     llm.RoleAssistant,
			Text:     resp.Text,
			ToolUses: resp.ToolUses,
		})

		if strings.TrimSpace(resp.Text) != "" && ui.OnAssistant != nil {
			ui.OnAssistant(resp.Text)
		}

		if len(resp.ToolUses) == 0 {
			return nil // end turn
		}

		results := make([]llm.ToolResult, 0, len(resp.ToolUses))
		for _, tu := range resp.ToolUses {
			content, isErr := a.execTool(ctx, tu, ui)
			results = append(results, llm.ToolResult{
				ToolUseID: tu.ID,
				Content:   content,
				IsError:   isErr,
			})
		}
		// 工具结果作为一条 user 消息回灌。
		a.append(llm.Message{Role: llm.RoleUser, ToolResults: results})
	}
}

func (a *Agent) toolDefs() []llm.Tool {
	list := a.tools.List()
	out := make([]llm.Tool, 0, len(list))
	for _, t := range list {
		out = append(out, llm.Tool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.Schema(),
		})
	}
	return out
}

func (a *Agent) execTool(ctx context.Context, tu llm.ToolUse, ui UI) (string, bool) {
	if ui.OnToolCall != nil && !ui.OnToolCall(tu.Name, tu.Input) {
		return "Tool call rejected by the user.", true
	}
	out, err := a.tools.Execute(ctx, tu.Name, tu.Input)
	if err != nil {
		msg := fmt.Sprintf("Error: %v", err)
		if ui.OnToolResult != nil {
			ui.OnToolResult(tu.Name, msg, true)
		}
		return msg, true
	}
	if ui.OnToolResult != nil {
		ui.OnToolResult(tu.Name, out, false)
	}
	return out, false
}
