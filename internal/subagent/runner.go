package subagent

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// 失控防线：单个子代理一拍最多调用模型次数；同时在跑的子代理上限。
const (
	maxCallsPerAgent = 30
	maxConcurrent    = 8
)

// nestedTools 是子代理注册表里剔除的工具（禁止嵌套生成子代理/工作流）。
var nestedTools = map[string]bool{"agent": true, "workflow": true}

// Runner 装配并运行子代理。外壳（TUI/plain）启动时注入 UI 工厂，
// 子代理的工具调用经它走与主 agent 相同的权限与显示通道。
type Runner struct {
	Client    func() (llm.LLM, string)                   // 主 agent 当前客户端（跟随 /model 热切换）
	Resolve   func(name string) (llm.LLM, string, error) // 解析 def.Model 覆盖，可为 nil
	Tools     *tools.Registry                            // 主注册表；spawn 时按 def.Tools 取子集
	MaxTokens int
	UI        func(label string) agent.UI // 外壳注入；nil 时子代理工具全拒（除 read）
	Log       func(text string)           // 进度旁白（workflow 的 log()），可为 nil

	defs []Def
	sem  chan struct{}
}

// NewRunner 创建子代理运行器。
func NewRunner(client func() (llm.LLM, string), reg *tools.Registry, maxTokens int, defs []Def) *Runner {
	return &Runner{
		Client:    client,
		Tools:     reg,
		MaxTokens: maxTokens,
		defs:      defs,
		sem:       make(chan struct{}, maxConcurrent),
	}
}

// Defs 返回全部子代理类型（/agents 列表与工具描述用）。
func (r *Runner) Defs() []Def { return r.defs }

func (r *Runner) lookup(name string) (Def, bool) {
	for _, d := range r.defs {
		if d.Name == name {
			return d, true
		}
	}
	return Def{}, false
}

// Spawn 启动一个子代理跑完委托任务，返回它的最终文本。
// 并发受信号量约束；ctx 取消（用户打断）原样传导给子代理。
func (r *Runner) Spawn(ctx context.Context, typ, prompt string) (string, error) {
	def, ok := r.lookup(typ)
	if !ok {
		names := make([]string, 0, len(r.defs))
		for _, d := range r.defs {
			names = append(names, d.Name)
		}
		return "", fmt.Errorf("未知子代理类型 %q（可用：%s）", typ, strings.Join(names, ", "))
	}
	return r.SpawnDef(ctx, def, prompt, SpawnOpts{})
}

// SpawnOpts 是 SpawnDef 的可选项。零值=与 Spawn 等价的默认行为。
type SpawnOpts struct {
	// Root 是子代理的工具根：文件工具锁定在此目录之内、相对路径基于它
	// 解析、bash 在它之下执行。空=主工作区（不限制）。
	Root string
	// Label 是 UI 显示标签；空=def.Name。
	Label string
	// UI 覆盖外壳注入的 UI 工厂（竞赛等聚合显示场景用）；nil=经 Runner.UI。
	// 注意 OnToolCall 为 nil 意味着工具全放行——调用方要自己保证沙箱（如 Root）。
	UI *agent.UI
	// NoSem 跳过运行器的并发信号量（调用方自管并发窗口，如竞赛模式）。
	NoSem bool
}

// SpawnDef 用一个临时定义启动子代理（不要求 def 在注册类型表里）。
// 竞赛等内置模式据此注入自己的契约提示与隔离写空间。
func (r *Runner) SpawnDef(ctx context.Context, def Def, prompt string, opts SpawnOpts) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("子代理任务 prompt 不能为空")
	}

	if !opts.NoSem {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	client, model := r.Client()
	if def.Model != "" {
		if r.Resolve == nil {
			return "", fmt.Errorf("子代理 %s 指定了模型 %q 但当前不支持模型解析", def.Name, def.Model)
		}
		c, m, err := r.Resolve(def.Model)
		if err != nil {
			return "", fmt.Errorf("子代理 %s 解析模型 %q: %w", def.Name, def.Model, err)
		}
		client, model = c, m
	}

	label := opts.Label
	if label == "" {
		label = def.Name
	}

	reg := r.subRegistry(def.Tools)
	if opts.Root != "" {
		reg.SetRoot(opts.Root)
	}
	sub := agent.New(client, reg, model, r.MaxTokens)
	sub.SetSystem(subSystem(def, opts.Root))
	sub.SetMaxCalls(maxCallsPerAgent)
	sub.SetUsageSource("subagent:" + label) // 记账来源：racer 即 subagent:racer#N
	ui := agent.UI{}
	switch {
	case opts.UI != nil:
		ui = *opts.UI
	case r.UI != nil:
		ui = r.UI(label)
	}
	var final string
	prev := ui.OnAssistant
	ui.OnAssistant = func(s string) { // 每段都覆盖，留下最后一段
		final = s
		if prev != nil {
			prev(s)
		}
	}
	if err := sub.Run(ctx, prompt, ui); err != nil {
		return "", fmt.Errorf("子代理 %s: %w", label, err)
	}
	if strings.TrimSpace(final) == "" {
		return "（子代理结束，无文本输出）", nil
	}
	return final, nil
}

// subRegistry 给子代理建工具子集：指定列表取交集，未指定取全部；
// 两种情况都剔除 agent/workflow（禁止嵌套）。MCP 工具同主注册表可用。
func (r *Runner) subRegistry(allowed []string) *tools.Registry {
	want := map[string]bool{}
	for _, n := range allowed {
		want[n] = true
	}
	sub := tools.NewRegistry()
	for _, t := range r.Tools.List() {
		if nestedTools[t.Name()] {
			continue
		}
		if len(allowed) > 0 && !want[t.Name()] {
			continue
		}
		sub.Add(t)
	}
	return sub
}

// subSystem 组装子代理系统提示：角色提示（定义正文）+ 子代理契约 + 环境。
// root 非空时作为子代理的工作目录展示（工具已被锁定在其中）。
func subSystem(d Def, root string) string {
	role := strings.TrimSpace(d.Prompt)
	if role == "" {
		role = "You are a focused sub-agent."
	}
	cwd := root
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			cwd = "."
		}
	}
	return role + fmt.Sprintf(`

You were spawned by a main agent to handle one delegated task. Work autonomously:
never ask questions, make reasonable assumptions. Your final message is returned
to the main agent verbatim as the task result, so end with a concise, complete report.

Environment:
- Working directory: %s
- OS: %s`, cwd, runtime.GOOS)
}
