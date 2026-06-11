// Package headless 实现无界面的单 turn 执行：`tokencode -p` 与 `tokencode serve`
// 共用这一套执行、事件与白名单语义，两条入口永不漂移。
//
// 设计要点：守卫放在工具层而非 UI 回调层——headless 下子代理的 UI 工厂为 nil
// 意味着工具全放行，只有把白名单包进工具本身（GateTool），主 agent 与经共享
// 注册表取子集的子代理才天然受同一约束。
package headless

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/tools"
)

// DefaultAllowed 是无人确认时默认放行的只读类工具白名单。
var DefaultAllowed = []string{"read", "websearch", "webfetch"}

// Allow 把白名单与 yolo 合成一个裁决函数（yolo 全放行）。
func Allow(allowed []string, yolo bool) func(name string) bool {
	set := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return func(name string) bool { return yolo || set[name] }
}

// GateTool 给工具包上 headless 守卫：裁决不过时不执行，返回错误
// "rejected (headless)" 作为 tool_result 喂回模型，让它自行调整方案。
func GateTool(t tools.Tool, allow func(name string) bool) tools.Tool {
	return gateTool{Tool: t, allow: allow}
}

type gateTool struct {
	tools.Tool
	allow func(string) bool
}

func (g gateTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	if !g.allow(g.Tool.Name()) {
		return "", errors.New("rejected (headless)")
	}
	return g.Tool.Execute(ctx, input)
}

// Concurrent 透传内层工具的并行安全标记（接口断言打在包装层，必须显式转发）。
func (g gateTool) Concurrent() bool {
	c, ok := g.Tool.(tools.Concurrent)
	return ok && c.Concurrent()
}

// Event 是执行过程中的一条事件，stream-json（JSONL）与 serve 的 SSE 共用。
// 流的最后一条恒为 type=result。
type Event struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`       // assistant_delta
	Name      string          `json:"name,omitempty"`       // tool_call / tool_result
	Input     json.RawMessage `json:"input,omitempty"`      // tool_call
	IsError   bool            `json:"is_error,omitempty"`   // tool_result / result
	Result    string          `json:"result,omitempty"`     // result
	ToolCalls *int            `json:"tool_calls,omitempty"` // result（指针：其余事件不带此字段）
}

// Result 是一次 headless 执行的汇总（-output json 与 serve 同步响应的结构）。
type Result struct {
	Result     string `json:"result"`
	Model      string `json:"model"`
	ToolCalls  int    `json:"tool_calls"`
	DurationMS int64  `json:"duration_ms"`
	IsError    bool   `json:"is_error"`
}

// Run 在给定 agent 上跑一个 turn 直到结束。onEvent 非 nil 时随执行过程回调
// 事件（含最后的 result 事件）；回调被互斥串行化，并行工具下也安全。
// 模型/网络错误不返回 error 而是落进 Result.IsError——调用方据此定退出码/状态。
func Run(ctx context.Context, ag *agent.Agent, model, prompt string, onEvent func(Event)) Result {
	start := time.Now()
	var (
		mu        sync.Mutex
		lastText  string
		toolCalls int
		streamed  bool // 当前 assistant 消息是否已走增量（避免 OnAssistant 重复发整段）
	)
	emit := func(ev Event) {
		if onEvent != nil {
			onEvent(ev)
		}
	}

	ui := agent.UI{
		OnAssistant: func(text string) {
			mu.Lock()
			defer mu.Unlock()
			if strings.TrimSpace(text) != "" {
				lastText = text
			}
			if streamed {
				streamed = false
				return
			}
			if strings.TrimSpace(text) != "" {
				emit(Event{Type: "assistant_delta", Text: text})
			}
		},
		// 守卫在工具层（GateTool），这里只记账与发事件，永远放行。
		OnToolCall: func(name string, input json.RawMessage) bool {
			mu.Lock()
			defer mu.Unlock()
			toolCalls++
			emit(Event{Type: "tool_call", Name: name, Input: input})
			return true
		},
		OnToolResult: func(name, result string, isErr bool) {
			mu.Lock()
			defer mu.Unlock()
			emit(Event{Type: "tool_result", Name: name, IsError: isErr})
		},
	}
	// 只在要事件流时开增量：text/json 模式没必要逼 codec 走 SSE。
	if onEvent != nil {
		ui.OnAssistantDelta = func(d string) {
			mu.Lock()
			defer mu.Unlock()
			streamed = true
			emit(Event{Type: "assistant_delta", Text: d})
		}
	}

	err := ag.Run(ctx, prompt, ui)
	res := Result{
		Result:     lastText,
		Model:      model,
		ToolCalls:  toolCalls,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		res.IsError = true
		res.Result = err.Error()
	}
	n := res.ToolCalls
	emit(Event{Type: "result", Result: res.Result, ToolCalls: &n, IsError: res.IsError})
	return res
}

// Execute 按输出格式跑一个 turn 并写 w：
//   - text：最终 assistant 文本（出错时不写 w，错误在 Result 里，由调用方走 stderr）；
//   - json：单个 Result 对象；
//   - stream-json：JSONL 事件流，最后一行恒为 result 事件。
func Execute(ctx context.Context, ag *agent.Agent, model, prompt, format string, w io.Writer) Result {
	switch format {
	case "json":
		res := Run(ctx, ag, model, prompt, nil)
		_ = json.NewEncoder(w).Encode(res)
		return res
	case "stream-json":
		enc := json.NewEncoder(w)
		return Run(ctx, ag, model, prompt, func(ev Event) { _ = enc.Encode(ev) })
	default: // text
		res := Run(ctx, ag, model, prompt, nil)
		if !res.IsError {
			fmt.Fprintln(w, res.Result)
		}
		return res
	}
}
