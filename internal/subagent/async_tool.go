package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yzfly/tokencode/internal/tools"
)

// AsyncTools 返回异步子代理四工具（spawn_agent / wait_agent / resume_agent /
// list_agents）。与同步 agent 工具互补：同步适合「拿到结果才能继续」，
// 异步适合「先派活、自己接着干、稍后收账」。
func AsyncTools(r *Runner) []tools.Tool {
	return []tools.Tool{spawnTool{r}, waitTool{r}, resumeTool{r}, listTool{r}}
}

// ---- spawn_agent ----

type spawnTool struct{ r *Runner }

func (spawnTool) Name() string { return "spawn_agent" }

func (t spawnTool) Concurrent() bool { return true }

func (t spawnTool) Description() string {
	return "Start a sub-agent in the background and return its handle id immediately. Keep working while it runs; collect the result later with wait_agent, continue it with resume_agent. Same agent types as the 'agent' tool."
}

func (t spawnTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"subagent_type": map[string]any{"type": "string", "description": "要使用的子代理类型名"},
			"prompt":        map[string]any{"type": "string", "description": "委托任务的完整描述（自包含：路径、要求、期望产出）"},
		},
		"required": []string{"subagent_type", "prompt"},
	}
}

func (t spawnTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var in struct {
		SubagentType string `json:"subagent_type"`
		Prompt       string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("spawn_agent: 参数解析失败: %w", err)
	}
	id, err := t.r.SpawnAsync(in.SubagentType, in.Prompt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已启动后台子代理 %s（%s）。用 wait_agent 取结果，list_agents 看状态。", id, in.SubagentType), nil
}

// ---- wait_agent ----

type waitTool struct{ r *Runner }

func (waitTool) Name() string { return "wait_agent" }

func (t waitTool) Concurrent() bool { return true }

func (t waitTool) Description() string {
	return "Block until a background sub-agent finishes and return its final report. Issue several wait_agent calls in one message to collect multiple agents in parallel."
}

func (t waitTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "spawn_agent 返回的句柄 id"},
		},
		"required": []string{"id"},
	}
}

func (t waitTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("wait_agent: 参数解析失败: %w", err)
	}
	return t.r.WaitJob(ctx, in.ID)
}

// ---- resume_agent ----

type resumeTool struct{ r *Runner }

func (resumeTool) Name() string { return "resume_agent" }

func (t resumeTool) Concurrent() bool { return true }

func (t resumeTool) Description() string {
	return "Send a follow-up prompt to a finished background sub-agent. It keeps its full context from previous turns; blocks until it replies."
}

func (t resumeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":     map[string]any{"type": "string", "description": "spawn_agent 返回的句柄 id"},
			"prompt": map[string]any{"type": "string", "description": "续聊内容（追问、修正、下一步）"},
		},
		"required": []string{"id", "prompt"},
	}
}

func (t resumeTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		ID     string `json:"id"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("resume_agent: 参数解析失败: %w", err)
	}
	return t.r.ResumeJob(ctx, in.ID, in.Prompt)
}

// ---- list_agents ----

type listTool struct{ r *Runner }

func (listTool) Name() string { return "list_agents" }

func (t listTool) Description() string {
	return "List background sub-agent handles with id, type, status (running/done/failed) and a result preview."
}

func (t listTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t listTool) Execute(context.Context, json.RawMessage) (string, error) {
	jobs := t.r.Jobs()
	if len(jobs) == 0 {
		return "（没有后台子代理）", nil
	}
	var b strings.Builder
	for _, j := range jobs {
		preview := strings.ReplaceAll(j.Result, "\n", " ")
		if rs := []rune(preview); len(rs) > 80 {
			preview = string(rs[:80]) + "…"
		}
		fmt.Fprintf(&b, "%s · %s · %s · %s\n", j.ID, j.Type, j.Status, preview)
	}
	return b.String(), nil
}
