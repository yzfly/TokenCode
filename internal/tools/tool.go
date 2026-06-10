// Package tools 实现 agent 可调用的内置工具（read/write/edit/bash）。
// 这一层与具体 LLM 协议无关：工具只认 JSON 参数，吐字符串结果。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool 是 agent 可调用的一个能力。
type Tool interface {
	Name() string
	Description() string
	// Schema 返回 JSON Schema，作为工具参数定义喂给模型。
	Schema() map[string]any
	// Execute 用模型给的 JSON 参数执行工具，返回喂回给模型的文本结果。
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// Registry 是工具注册表，保持注册顺序。
type Registry struct {
	tools map[string]Tool
	order []string
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
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Get 按名取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List 按注册顺序返回所有工具。
func (r *Registry) List() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// Execute 找到并执行工具；找不到返回错误。
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, input)
}
