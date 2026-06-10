package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/subagent"
	"github.com/yzfly/tokencode/internal/tools"
)

// echoLLM 把收到的用户消息原样回声（前缀 echo:），便于校验编排路径。
type echoLLM struct{}

func (echoLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	last := req.Messages[len(req.Messages)-1].Text
	return llm.Response{Text: "echo:" + last, StopReason: "end_turn"}, nil
}

func newTestTool(t *testing.T) (*wfTool, *[]string) {
	t.Helper()
	r := subagent.NewRunner(
		func() (llm.LLM, string) { return echoLLM{}, "test" },
		tools.NewRegistry(tools.Read()),
		512,
		subagent.Builtins(),
	)
	var logs []string
	r.Log = func(s string) { logs = append(logs, s) }
	return NewTool(r), &logs
}

func run(t *testing.T, tool *wfTool, script string) (string, error) {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"script": script})
	return tool.Execute(context.Background(), in)
}

func TestWorkflowAgentAndLog(t *testing.T) {
	tool, logs := newTestTool(t)
	out, err := run(t, tool, `
		log("开始");
		const r = agent("explore", "任务A");
		"结果=" + r;
	`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "结果=echo:任务A" {
		t.Errorf("out = %q", out)
	}
	if len(*logs) != 1 || (*logs)[0] != "开始" {
		t.Errorf("logs = %v", *logs)
	}
}

func TestWorkflowParallelKeepsOrder(t *testing.T) {
	tool, _ := newTestTool(t)
	out, err := run(t, tool, `
		const rs = parallel([1,2,3,4,5].map(i => ({type: "explore", prompt: "t" + i})));
		rs.join("|");
	`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "echo:t1|echo:t2|echo:t3|echo:t4|echo:t5" {
		t.Errorf("out = %q", out)
	}
}

func TestWorkflowScriptErrorSurfaces(t *testing.T) {
	tool, _ := newTestTool(t)
	_, err := run(t, tool, `throw new Error("炸了")`)
	if err == nil || !strings.Contains(err.Error(), "炸了") {
		t.Fatalf("want script error, got %v", err)
	}
}

func TestWorkflowAgentErrorThrowsCatchable(t *testing.T) {
	tool, _ := newTestTool(t)
	out, err := run(t, tool, `
		let msg = "";
		try { agent("不存在的类型", "x"); } catch (e) { msg = "caught"; }
		msg;
	`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "caught" {
		t.Errorf("out = %q", out)
	}
}

func TestWorkflowUndefinedResult(t *testing.T) {
	tool, _ := newTestTool(t)
	// 整个脚本没有产生 completion value（只有声明）→ 返回兜底完成信息。
	out, err := run(t, tool, `let unused = 1;`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "工作流完成") {
		t.Errorf("out = %q", out)
	}
}

func TestWorkflowInterrupt(t *testing.T) {
	tool, _ := newTestTool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 直接取消：脚本应立即被打断
	in, _ := json.Marshal(map[string]string{"script": `while(true){}`})
	_, err := tool.Execute(ctx, in)
	if err == nil {
		t.Fatal("want interrupt error")
	}
}
