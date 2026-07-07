// Package tools 实现 agent 可调用的内置工具（read/write/edit/bash）。
// 这一层与具体 LLM 协议无关：工具只认 JSON 参数，吐字符串结果。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// readOnlyTools 是只读工具集：不改文件、不改外部状态，权限层可免确认放行
// （plan 模式、非交互拍、headless 默认白名单共用这一份判定）。
var readOnlyTools = map[string]bool{
	"read": true, "ls": true, "glob": true, "grep": true,
	"git_status": true, "git_diff": true,
	"websearch": true, "webfetch": true,
	"cron_list": true, "list_agents": true,
}

// ReadOnly 报告工具是否只读（对文件系统与外部世界无副作用）。
func ReadOnly(name string) bool { return readOnlyTools[name] }

// Tool 是 agent 可调用的一个能力。
type Tool interface {
	Name() string
	Description() string
	// Schema 返回 JSON Schema，作为工具参数定义喂给模型。
	Schema() map[string]any
	// Execute 用模型给的 JSON 参数执行工具，返回喂回给模型的文本结果。
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// Concurrent 是工具的可选标记：模型在一条消息里发多个调用时，
// 实现了它且返回 true 的工具（如子代理）并行执行，其余仍按序。
type Concurrent interface {
	Concurrent() bool
}

// Registry 是工具注册表，保持注册顺序。
// 并发安全：MCP server 在后台连上后会动态 Add，agent 同时在读。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	order []string
	root  string // per-agent 工具根；空=不限制（主 agent）
	// checkpoint 是可选的写盘前快照钩子：write/edit 在覆盖文件前回调它
	// 记录原内容（/rewind 的物质基础）。Registry 级字段而非全局——竞赛
	// racer 的注册表有 worktree 隔离不需要、也不该误触主仓库的检查点。
	checkpoint func(tool, path string)
}

// NewRegistry 用给定工具建一个注册表。
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

// Add 注册一个工具（重名覆盖，顺序不变）。
func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List 按注册顺序返回所有工具。
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// SetRoot 绑定 per-agent 工具根：文件工具的相对路径基于它解析且只允许
// 访问它之内，bash 在它之下执行。在注册表投入使用前设置一次。
func (r *Registry) SetRoot(root string) { r.root = root }

// Root 返回注册表绑定的工具根（空=不限制）。
func (r *Registry) Root() string { return r.root }

// SetCheckpointer 挂上写盘前快照钩子（nil=关闭）。fn 收到工具名与解析后的
// 绝对路径，在文件被覆盖/创建之前调用。注意 bash 不经此钩子（已知盲区）。
func (r *Registry) SetCheckpointer(fn func(tool, path string)) { r.checkpoint = fn }

// Checkpointer 返回当前挂载的快照钩子（nil=未开启）。
func (r *Registry) Checkpointer() func(tool, path string) { return r.checkpoint }

// Execute 找到并执行工具；找不到返回错误。注册表绑定了根时注入 ctx，
// 文件/bash 工具据此解析与守卫路径。
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	if r.root != "" {
		ctx = WithRoot(ctx, r.root)
	}
	if fn := r.checkpoint; fn != nil {
		// 绑定工具名后注入 ctx，write/edit 写盘前回调（见 guard.go）。
		ctx = withCheckpoint(ctx, func(path string) { fn(name, path) })
	}
	return t.Execute(ctx, input)
}
