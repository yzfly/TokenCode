package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// agentTool 是暴露给模型的 agent 工具：把任务委托给隔离上下文的子代理。
type agentTool struct {
	r *Runner
}

// NewTool 用运行器构造 agent 工具（注册进主 agent 的工具表）。
func NewTool(r *Runner) *agentTool { return &agentTool{r: r} }

func (t *agentTool) Name() string { return "agent" }

// Concurrent 标记并行安全：一条消息里的多个 agent 调用并发执行。
func (t *agentTool) Concurrent() bool { return true }

func (t *agentTool) Description() string {
	var b strings.Builder
	b.WriteString(`Delegate a task to a sub-agent running in its own isolated context.
The sub-agent works autonomously with its own tools and only its final report comes
back as this tool's result — intermediate file dumps and tool noise stay out of your
context. Use it for self-contained subtasks, broad searches, or independent workstreams.
Launch several agent calls in ONE message to run them in parallel. Sub-agents cannot
spawn further sub-agents, cannot see your conversation, and cannot ask the user
questions — make each prompt self-contained with all needed paths and context.

Available agent types:`)
	for _, d := range t.r.Defs() {
		fmt.Fprintf(&b, "\n- %s: %s", d.Name, d.Description)
	}
	return b.String()
}

func (t *agentTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent_type": map[string]any{
				"type":        "string",
				"description": "要使用的子代理类型名",
			},
			"prompt": map[string]any{
				"type":        "string",
				"description": "委托任务的完整描述（自包含：路径、要求、期望产出）",
			},
		},
		"required": []string{"subagent_type", "prompt"},
	}
}

func (t *agentTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		SubagentType string `json:"subagent_type"`
		Prompt       string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("agent: 参数解析失败: %w", err)
	}
	return t.r.Spawn(ctx, in.SubagentType, in.Prompt)
}
