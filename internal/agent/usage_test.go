package agent

import (
	"context"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
	"github.com/yzfly/tokencode/internal/usage"
)

// TestRunTurnLogsUsage 验证 runTurn 是统一记账拦截点：带用量的响应落进账本，
// Source 跟随 SetUsageSource；不报用量的端点（全零）不产生记录。
func TestRunTurnLogsUsage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	fake := &fakeLLM{responses: []llm.Response{{
		Text:       "ok",
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 7, CacheReadTokens: 50, CacheWriteTokens: 8},
	}}}
	a := New(fake, tools.NewRegistry(), "test-model", 64)
	a.SetUsageSource("subagent:tester")
	if err := a.Run(context.Background(), "hi", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	now := time.Now()
	sum, err := usage.Summarize(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.Total.Calls != 1 || sum.Total.In != 100 || sum.Total.Out != 7 ||
		sum.Total.CacheRead != 50 || sum.Total.CacheWrite != 8 {
		t.Fatalf("total wrong: %+v", sum.Total)
	}
	if b := sum.BySource["subagent:tester"]; b.Calls != 1 {
		t.Fatalf("source wrong: %+v", sum.BySource)
	}
	if b := sum.ByModel["test-model"]; b.Calls != 1 {
		t.Fatalf("model wrong: %+v", sum.ByModel)
	}
}

// TestRunTurnSkipsZeroUsage 验证端点不报用量时不写账本（也保护既有
// fake 测试不污染真实账本）。
func TestRunTurnSkipsZeroUsage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	fake := &fakeLLM{responses: []llm.Response{{Text: "ok", StopReason: llm.StopEndTurn}}}
	a := New(fake, tools.NewRegistry(), "test-model", 64)
	if err := a.Run(context.Background(), "hi", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	now := time.Now()
	sum, err := usage.Summarize(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.Total.Calls != 0 {
		t.Fatalf("零用量不该入账: %+v", sum.Total)
	}
}
