package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/tools"
)

// Tools 返回定时任务三工具（cron_create / cron_list / cron_delete）。
func Tools(m *Manager) []tools.Tool {
	return []tools.Tool{createTool{m}, listTool{m}, deleteTool{m}}
}

// ---- cron_create ----

type createTool struct{ m *Manager }

func (createTool) Name() string { return "cron_create" }

func (createTool) Description() string {
	return "Schedule a recurring in-session task: every <interval> the prompt is queued as a new turn for the main agent. Tasks live only in this session (not persisted) and scheduled turns run read-only (no writes without the user present). Minimum interval 1m."
}

func (createTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":   map[string]any{"type": "string", "description": "Unique task name"},
			"every":  map[string]any{"type": "string", "description": "Interval as Go duration, e.g. '5m', '1h' (minimum 1m)"},
			"prompt": map[string]any{"type": "string", "description": "Prompt queued to the main agent on each firing"},
		},
		"required": []string{"name", "every", "prompt"},
	}
}

func (t createTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Name   string `json:"name"`
		Every  string `json:"every"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	every, err := time.ParseDuration(a.Every)
	if err != nil {
		return "", fmt.Errorf("every 解析失败（要 Go duration，如 '5m'）: %w", err)
	}
	if err := t.m.Create(a.Name, every, a.Prompt); err != nil {
		return "", err
	}
	return fmt.Sprintf("定时任务 %q 已创建：每 %s 触发一次。", a.Name, every), nil
}

// ---- cron_list ----

type listTool struct{ m *Manager }

func (listTool) Name() string { return "cron_list" }

func (listTool) Description() string {
	return "List scheduled in-session tasks with interval, run count and next fire time."
}

func (listTool) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t listTool) Execute(context.Context, json.RawMessage) (string, error) {
	ents := t.m.List()
	if len(ents) == 0 {
		return "（没有定时任务）", nil
	}
	var b strings.Builder
	for _, e := range ents {
		fmt.Fprintf(&b, "%s · 每 %s · 已跑 %d 次 · 下次 %s · %s\n",
			e.Name, e.Every, e.Runs, e.NextAt.Format("15:04:05"), e.Prompt)
	}
	return b.String(), nil
}

// ---- cron_delete ----

type deleteTool struct{ m *Manager }

func (deleteTool) Name() string { return "cron_delete" }

func (deleteTool) Description() string {
	return "Delete a scheduled in-session task by name."
}

func (deleteTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "description": "Task name to delete"},
		},
		"required": []string{"name"},
	}
}

func (t deleteTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if err := t.m.Delete(a.Name); err != nil {
		return "", err
	}
	return fmt.Sprintf("定时任务 %q 已删除。", a.Name), nil
}
