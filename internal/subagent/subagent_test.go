package subagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeLLM 按脚本依次返回预设响应。
type fakeLLM struct {
	responses []llm.Response
	calls     int
}

func (f *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	r := f.responses[f.calls%len(f.responses)]
	f.calls++
	return r, nil
}

func TestDiscoverCustomDef(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, ".tokencode", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := `---
name: reviewer
description: "代码审查专家"
tools: read, bash
model: cheap
---

You are a code reviewer.`
	if err := os.WriteFile(filepath.Join(agentsDir, "reviewer.md"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}

	defs := Discover(dir)
	var got *Def
	for i := range defs {
		if defs[i].Name == "reviewer" {
			got = &defs[i]
		}
	}
	if got == nil {
		t.Fatalf("custom def not discovered: %+v", defs)
	}
	if got.Description != "代码审查专家" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.Tools) != 2 || got.Tools[0] != "read" || got.Tools[1] != "bash" {
		t.Errorf("tools = %v", got.Tools)
	}
	if got.Model != "cheap" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Prompt != "You are a code reviewer." {
		t.Errorf("prompt = %q", got.Prompt)
	}

	// 内置类型也在。
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["explore"] || !names["general-purpose"] {
		t.Errorf("builtins missing: %v", names)
	}
}

func TestSubRegistryExcludesNesting(t *testing.T) {
	reg := tools.NewRegistry(tools.Read(), tools.Write(), fakeTool{name: "agent"}, fakeTool{name: "workflow"})
	r := NewRunner(nil, reg, 1024, Builtins())

	// 默认（全工具）：剔除 agent/workflow。
	sub := r.subRegistry(nil)
	if _, ok := sub.Get("agent"); ok {
		t.Fatal("agent must not nest")
	}
	if _, ok := sub.Get("workflow"); ok {
		t.Fatal("workflow must not nest")
	}
	if _, ok := sub.Get("write"); !ok {
		t.Fatal("write should be available by default")
	}

	// 指定列表：取交集且仍剔除嵌套工具。
	sub = r.subRegistry([]string{"read", "agent"})
	if _, ok := sub.Get("read"); !ok {
		t.Fatal("read should be in subset")
	}
	if _, ok := sub.Get("write"); ok {
		t.Fatal("write not in subset")
	}
	if _, ok := sub.Get("agent"); ok {
		t.Fatal("agent must not nest even if listed")
	}
}

type fakeTool struct{ name string }

func (f fakeTool) Name() string           { return f.name }
func (f fakeTool) Description() string    { return f.name }
func (f fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (f fakeTool) Execute(ctx context.Context, in json.RawMessage) (string, error) {
	return "ok", nil
}

func TestSpawnReturnsFinalText(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "探索完成：找到 3 处。", StopReason: "end_turn"}}}
	reg := tools.NewRegistry(tools.Read())
	r := NewRunner(func() (llm.LLM, string) { return fake, "test-model" }, reg, 1024, Builtins())

	out, err := r.Spawn(context.Background(), "explore", "找 TODO")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if out != "探索完成：找到 3 处。" {
		t.Errorf("out = %q", out)
	}
}

func TestSpawnUnknownType(t *testing.T) {
	r := NewRunner(nil, tools.NewRegistry(), 1024, Builtins())
	_, err := r.Spawn(context.Background(), "nope", "x")
	if err == nil || !strings.Contains(err.Error(), "未知子代理类型") {
		t.Fatalf("want unknown-type error, got %v", err)
	}
}

func TestAgentToolDescriptionListsTypes(t *testing.T) {
	r := NewRunner(nil, tools.NewRegistry(), 1024, Builtins())
	desc := NewTool(r).Description()
	if !strings.Contains(desc, "explore") || !strings.Contains(desc, "general-purpose") {
		t.Errorf("description missing types: %s", desc)
	}
}
