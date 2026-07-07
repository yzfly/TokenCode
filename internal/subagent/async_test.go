package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

func TestSpawnAsyncWaitResume(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "第一拍完成。", StopReason: "end_turn"}}}
	reg := tools.NewRegistry(tools.Read())
	r := NewRunner(func() (llm.LLM, string) { return fake, "test-model" }, reg, 1024, Builtins())

	id, err := r.SpawnAsync("explore", "找 TODO")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if id != "ag-1" {
		t.Errorf("id = %q", id)
	}

	out, err := r.WaitJob(context.Background(), id)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if out != "第一拍完成。" {
		t.Errorf("wait result = %q", out)
	}

	// 结束后可再 wait（结果稳定）。
	if out, _ := r.WaitJob(context.Background(), id); out != "第一拍完成。" {
		t.Errorf("second wait = %q", out)
	}

	// resume 在同一实例上续拍。
	fake.responses = []llm.Response{{Text: "续拍完成。", StopReason: "end_turn"}}
	out, err = r.ResumeJob(context.Background(), id, "继续")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out != "续拍完成。" {
		t.Errorf("resume result = %q", out)
	}

	jobs := r.Jobs()
	if len(jobs) != 1 || jobs[0].Status != JobDone || !strings.Contains(jobs[0].Result, "续拍") {
		t.Errorf("jobs = %+v", jobs)
	}
}

func TestSpawnAsyncUnknownType(t *testing.T) {
	r := NewRunner(nil, tools.NewRegistry(), 1024, Builtins())
	if _, err := r.SpawnAsync("nope", "x"); err == nil || !strings.Contains(err.Error(), "未知子代理类型") {
		t.Fatalf("want unknown-type error, got %v", err)
	}
}

func TestWaitJobUnknownID(t *testing.T) {
	r := NewRunner(nil, tools.NewRegistry(), 1024, Builtins())
	if _, err := r.WaitJob(context.Background(), "ag-99"); err == nil {
		t.Fatal("unknown id must fail")
	}
}

func TestWaitJobRespectsCtxCancel(t *testing.T) {
	// 一个永不返回的 LLM：wait 的 ctx 取消要能解除阻塞，后台代理继续挂着。
	block := make(chan struct{})
	stuck := blockingLLM{block: block}
	defer close(block)

	r := NewRunner(func() (llm.LLM, string) { return stuck, "m" }, tools.NewRegistry(tools.Read()), 1024, Builtins())
	id, err := r.SpawnAsync("explore", "hang")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.WaitJob(ctx, id); err == nil {
		t.Fatal("cancelled wait must error")
	}
}

type blockingLLM struct{ block chan struct{} }

func (b blockingLLM) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	select {
	case <-b.block:
	case <-ctx.Done():
	}
	return llm.Response{Text: "late", StopReason: "end_turn"}, nil
}
