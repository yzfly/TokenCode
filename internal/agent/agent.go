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
	// OnAssistantDelta 在流式生成时随每段正文增量调用（最终完整文本仍经
	// OnAssistant 给出一次）。为 nil 时走非流式 Complete。
	OnAssistantDelta func(delta string)
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
	system    string // 自定义系统提示（子代理用）；空=默认 SystemPrompt()
	maxCalls  int    // 单 turn 模型调用次数上限；0=不限（子代理防失控用）

	mu      sync.Mutex
	msgs    []llm.Message
	running atomic.Bool // 是否有 turn 正在执行（仅 Serve 路径维护）

	persist   func([]llm.Message) // 持久化回调，可为 nil
	persisted int                 // 已交给 persist 的历史水位线
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
	err := a.runTurn(ctx, userInput, ui)
	a.flushPersist()
	return err
}

// SetClient 运行时切换模型/协议客户端（/model 命令）。turn 进行中调用也安全：
// 当前请求用旧客户端，下一次模型调用起用新的。
func (a *Agent) SetClient(client llm.LLM, model string) {
	a.mu.Lock()
	a.llm = client
	a.model = model
	a.mu.Unlock()
}

// Client 返回当前客户端与模型。子代理用它继承主 agent 的模型（含热切换后的）。
func (a *Agent) Client() (llm.LLM, string) {
	return a.client()
}

// SetSystem 覆盖系统提示（子代理用自己的角色提示替代默认 SystemPrompt）。
func (a *Agent) SetSystem(s string) {
	a.system = s
}

// SetMaxCalls 限制单 turn 的模型调用次数（0=不限）。子代理用它防失控循环。
func (a *Agent) SetMaxCalls(n int) {
	a.maxCalls = n
}

// client 取当前客户端与模型（每次模型调用前读一次，配合 SetClient）。
func (a *Agent) client() (llm.LLM, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.llm, a.model
}

// Seed 在 Serve/Run 启动前注入历史（resume）。注入的消息视为已持久化。
func (a *Agent) Seed(ms []llm.Message) {
	a.mu.Lock()
	a.msgs = append(a.msgs[:0], ms...)
	a.persisted = len(a.msgs)
	a.mu.Unlock()
}

// SetPersist 注册持久化回调：每拍结束后，新留入历史的消息按序交给它
// （在 turn 执行者 goroutine 上同步调用）。心跳空转等被剔除的拍不会经过它。
func (a *Agent) SetPersist(fn func([]llm.Message)) {
	a.persist = fn
}

// flushPersist 把水位线之上的新消息交给持久化回调。truncate 把历史截到
// 水位线之下时（空转剔除/失败回滚），水位线跟着回落，被剔除的消息从未落盘。
func (a *Agent) flushPersist() {
	if a.persist == nil {
		return
	}
	a.mu.Lock()
	if a.persisted > len(a.msgs) {
		a.persisted = len(a.msgs)
	}
	fresh := make([]llm.Message, len(a.msgs)-a.persisted)
	copy(fresh, a.msgs[a.persisted:])
	a.persisted = len(a.msgs)
	a.mu.Unlock()
	if len(fresh) > 0 {
		a.persist(fresh)
	}
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
	system := a.system
	if system == "" {
		system = SystemPrompt()
	}
	a.append(llm.Message{Role: llm.RoleUser, Text: userInput})

	calls := 0
	for {
		if a.maxCalls > 0 && calls >= a.maxCalls {
			return fmt.Errorf("超出单 turn 模型调用上限（%d 次），任务可能失控，已终止", a.maxCalls)
		}
		calls++
		if ui.OnThinking != nil {
			ui.OnThinking(true)
		}
		client, model := a.client()
		req := llm.Request{
			Model:     model,
			System:    system,
			Messages:  a.history(),
			Tools:     a.toolDefs(),
			MaxTokens: a.maxTokens,
		}
		var resp llm.Response
		var err error
		// codec 支持流式且外壳要增量时走流式；两条路径返回同语义的 Response。
		if s, ok := client.(llm.Streamer); ok && ui.OnAssistantDelta != nil {
			resp, err = s.CompleteStream(ctx, req, func(d llm.Delta) {
				if d.Text != "" {
					ui.OnAssistantDelta(d.Text)
				}
			})
		} else {
			resp, err = client.Complete(ctx, req)
		}
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

		// 并行安全的工具（子代理）并发执行，其余按模型给出的顺序串行；
		// 结果按原顺序回灌，模型看不出执行方式的差别。
		results := make([]llm.ToolResult, len(resp.ToolUses))
		var wg sync.WaitGroup
		for i, tu := range resp.ToolUses {
			if len(resp.ToolUses) > 1 && a.concurrentTool(tu.Name) {
				wg.Add(1)
				go func(i int, tu llm.ToolUse) {
					defer wg.Done()
					content, isErr := a.execTool(ctx, tu, ui)
					results[i] = llm.ToolResult{ToolUseID: tu.ID, Content: content, IsError: isErr}
				}(i, tu)
				continue
			}
			content, isErr := a.execTool(ctx, tu, ui)
			results[i] = llm.ToolResult{ToolUseID: tu.ID, Content: content, IsError: isErr}
		}
		wg.Wait()
		// 工具结果作为一条 user 消息回灌。
		a.append(llm.Message{Role: llm.RoleUser, ToolResults: results})
	}
}

// concurrentTool 报告一个工具是否标记了并行安全（tools.Concurrent）。
func (a *Agent) concurrentTool(name string) bool {
	t, ok := a.tools.Get(name)
	if !ok {
		return false
	}
	c, ok := t.(tools.Concurrent)
	return ok && c.Concurrent()
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
