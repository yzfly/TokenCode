// Package workflow 实现动态工作流编排：模型写一段 JavaScript 脚本，
// 由 goja 在隔离 VM 里执行，脚本通过三个原语驱动子代理——
//
//	agent(type, prompt)      串行启动一个子代理，返回其最终文本
//	parallel([{type,prompt}]) 并发启动一批子代理，按序返回各自结果
//	log(msg)                 给用户的进度旁白
//
// 决策权倒置是它和 agent 工具的本质区别：多步编排（循环、扇出、聚合、
// 条件分支）由脚本确定性控制，而不是模型每轮重新决定——中间结果存在
// 脚本变量里，不占模型上下文。
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dop251/goja"

	"github.com/yzfly/tokencode/internal/subagent"
)

// maxAgentsPerRun 是单次工作流可启动的子代理总数上限（失控防线）。
const maxAgentsPerRun = 100

// wfTool 是暴露给模型的 workflow 工具。
type wfTool struct {
	r *subagent.Runner
}

// NewTool 构造 workflow 工具（与子代理共享运行器：并发上限、权限、显示同源）。
func NewTool(r *subagent.Runner) *wfTool { return &wfTool{r: r} }

func (t *wfTool) Name() string { return "workflow" }

func (t *wfTool) Description() string {
	var b strings.Builder
	b.WriteString(`Run a JavaScript orchestration script that coordinates multiple sub-agents
deterministically. Use this instead of the agent tool when the work needs loops,
conditionals, fan-out over a list, or aggregation across many agents — the script
holds intermediate results in variables so they never bloat your context.

Script API (synchronous, no async/await/Promise/setTimeout):
  agent(type, prompt) -> string   run one sub-agent, blocks until done, returns its final report; throws on failure
  parallel(tasks) -> string[]     tasks = [{type, prompt}, ...]; runs them concurrently, returns results in order (a failed item returns "[agent error] ...")
  log(msg)                        progress note shown to the user

The value of the script's last expression is returned as this tool's result.
No filesystem/network access from the script itself — all real work goes through agents.
At most ` + fmt.Sprint(maxAgentsPerRun) + ` agents per run.

Agent types available to agent()/parallel():`)
	for _, d := range t.r.Defs() {
		fmt.Fprintf(&b, "\n- %s: %s", d.Name, d.Description)
	}
	b.WriteString(`

Example — fan out a review then synthesize:
  const files = ["a.go", "b.go", "c.go"];
  const reviews = parallel(files.map(f => ({type: "explore", prompt: "Review " + f + " for bugs, report findings with line numbers"})));
  log("reviews done");
  agent("general-purpose", "Synthesize these review findings into one prioritized list:\n" + reviews.join("\n---\n"));`)
	return b.String()
}

func (t *wfTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"script": map[string]any{
				"type":        "string",
				"description": "要执行的 JavaScript 编排脚本",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "一句话说明这个工作流做什么（显示给用户）",
			},
		},
		"required": []string{"script"},
	}
}

func (t *wfTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Script      string `json:"script"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("workflow: 参数解析失败: %w", err)
	}
	if strings.TrimSpace(in.Script) == "" {
		return "", fmt.Errorf("workflow: script 不能为空")
	}
	return t.run(ctx, in.Script)
}

// run 在一个新 VM 里执行脚本。ctx 取消（用户打断）通过 vm.Interrupt 立即中断脚本。
func (t *wfTool) run(ctx context.Context, script string) (string, error) {
	vm := goja.New()
	// JS 侧用小写字段名（{type, prompt}），按 json tag 映射到 Go 结构。
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
	var spawned atomic.Int64

	// 用户打断 → 中断 VM。脚本结束后停止监听。
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt("interrupted")
		case <-done:
		}
	}()

	spawn := func(typ, prompt string) (string, error) {
		if spawned.Add(1) > maxAgentsPerRun {
			return "", fmt.Errorf("工作流超出子代理总数上限（%d）", maxAgentsPerRun)
		}
		return t.r.Spawn(ctx, typ, prompt)
	}

	mustSet := func(name string, fn any) {
		if err := vm.Set(name, fn); err != nil {
			panic(err) // vm.Set 对合法函数不会失败
		}
	}

	mustSet("agent", func(call goja.FunctionCall) goja.Value {
		typ := call.Argument(0).String()
		prompt := call.Argument(1).String()
		out, err := spawn(typ, prompt)
		if err != nil {
			panic(vm.ToValue(err.Error())) // 抛成 JS 异常，脚本可 try/catch
		}
		return vm.ToValue(out)
	})

	mustSet("parallel", func(call goja.FunctionCall) goja.Value {
		var tasks []struct {
			Type   string `json:"type"`
			Prompt string `json:"prompt"`
		}
		if err := vm.ExportTo(call.Argument(0), &tasks); err != nil {
			panic(vm.ToValue("parallel: 参数须为 [{type, prompt}, ...]: " + err.Error()))
		}
		results := make([]string, len(tasks))
		var wg sync.WaitGroup
		for i, task := range tasks {
			wg.Add(1)
			go func(i int, typ, prompt string) {
				defer wg.Done()
				out, err := spawn(typ, prompt)
				if err != nil {
					out = "[agent error] " + err.Error()
				}
				results[i] = out
			}(i, task.Type, task.Prompt)
		}
		wg.Wait()
		return vm.ToValue(results)
	})

	mustSet("log", func(msg string) {
		if t.r.Log != nil {
			t.r.Log(msg)
		}
	})

	v, err := vm.RunString(script)
	if err != nil {
		// 打断和脚本异常都走这里；打断时报 ctx 的取消原因更准确。
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("workflow 脚本错误: %w", err)
	}
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return fmt.Sprintf("（工作流完成，共启动 %d 个子代理）", spawned.Load()), nil
	}
	return v.String(), nil
}
