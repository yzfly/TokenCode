package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// TestEstimateTokens 验证启发式估算：中英混合下偏保守（不低于经验真值），
// 工具调用 JSON 与消息常数开销都计入。
func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Fatalf("空历史应为 0，got %d", got)
	}

	// 英文 400 字符 ≈ 100 tokens（4 字符/token 经验值），估算应 ≥ 真值且不离谱。
	en := strings.Repeat("word ", 80)
	gotEN := EstimateTokens([]llm.Message{{Role: llm.RoleUser, Text: en}})
	if gotEN < 100 || gotEN > 250 {
		t.Fatalf("英文估算 %d 超出保守区间 [100,250]", gotEN)
	}

	// 中文 100 字（300 字节）≈ 67 tokens（1.5 字/token 经验值），同样宁高勿低。
	zh := strings.Repeat("压缩上下文历史摘要长会话", 8) + "压缩上下"
	if n := len([]rune(zh)); n != 100 {
		t.Fatalf("用例自检：中文应为 100 字，got %d", n)
	}
	gotZH := EstimateTokens([]llm.Message{{Role: llm.RoleUser, Text: zh}})
	if gotZH < 67 || gotZH > 180 {
		t.Fatalf("中文估算 %d 超出保守区间 [67,180]", gotZH)
	}

	// 中英混合 = 各部分之和量级；工具调用与结果计入后应严格变大。
	mixed := []llm.Message{
		{Role: llm.RoleUser, Text: en + zh},
		{Role: llm.RoleAssistant, Text: "ok", ToolUses: []llm.ToolUse{
			{ID: "c1", Name: "bash", Input: []byte(`{"cmd":"go test ./... 全部跑一遍"}`)},
		}},
		{Role: llm.RoleUser, ToolResults: []llm.ToolResult{
			{ToolUseID: "c1", Content: strings.Repeat("PASS\n", 40)},
		}},
	}
	gotMixed := EstimateTokens(mixed)
	if gotMixed <= gotEN+gotZH {
		t.Fatalf("混合+工具估算 %d 应大于纯文本之和 %d", gotMixed, gotEN+gotZH)
	}
	noTool := EstimateTokens(mixed[:1])
	if gotMixed <= noTool {
		t.Fatalf("工具 JSON 未计入：%d <= %d", gotMixed, noTool)
	}
}

// seedThreeTurns 注入 3 个完整轮次（6 条消息），第 2 轮带工具调用与回灌，
// 验证压缩切点能跳过工具结果型 user 消息。
func seedThreeTurns(a *Agent) {
	a.Seed([]llm.Message{
		{Role: llm.RoleUser, Text: "turn1: 改 internal/foo.go"},
		{Role: llm.RoleAssistant, Text: "turn1 done"},
		{Role: llm.RoleUser, Text: "turn2: 跑测试"},
		{Role: llm.RoleAssistant, ToolUses: []llm.ToolUse{{ID: "c1", Name: "bash", Input: []byte(`{}`)}}},
		{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{ToolUseID: "c1", Content: "PASS"}}},
		{Role: llm.RoleAssistant, Text: "turn2 done"},
		{Role: llm.RoleUser, Text: "turn3: 收尾"},
		{Role: llm.RoleAssistant, Text: "turn3 done"},
	})
}

// TestCompactRewritesHistory 验证：压缩后历史 = 摘要 + 最近 2 个完整轮次；
// 摘要请求只含被压缩段 + 指令（不带工具定义），用户侧重点进入指令。
func TestCompactRewritesHistory(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "目标：改 foo.go；已完成。", StopReason: llm.StopEndTurn}}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	seedThreeTurns(a)

	n, err := a.Compact(context.Background(), "保留文件路径")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 2 {
		t.Fatalf("应压缩 turn1 的 2 条消息，got %d", n)
	}

	msgs := a.Snapshot()
	if len(msgs) != 7 { // 摘要 + turn2 的 4 条 + turn3 的 2 条
		t.Fatalf("压缩后应 7 条，got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != llm.RoleUser || !strings.HasPrefix(msgs[0].Text, SummaryPrefix) {
		t.Fatalf("第 0 条应是摘要 user 消息，got %+v", msgs[0])
	}
	if !strings.Contains(msgs[0].Text, "目标：改 foo.go") {
		t.Fatalf("摘要正文未并入：%q", msgs[0].Text)
	}
	if msgs[1].Text != "turn2: 跑测试" || msgs[6].Text != "turn3 done" {
		t.Fatalf("保留轮次形态不对: %+v", msgs[1:])
	}

	// 摘要请求：被压缩的 2 条 + 1 条指令，指令带用户侧重点，不带工具。
	req := fake.lastReq
	if len(req.Messages) != 3 || req.Messages[0].Text != "turn1: 改 internal/foo.go" {
		t.Fatalf("摘要请求消息不对: %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[2].Text, "保留文件路径") {
		t.Fatalf("用户侧重点未进指令: %q", req.Messages[2].Text)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("摘要请求不应带工具，got %d", len(req.Tools))
	}
}

// TestCompactTooShort 验证：不足 keepRecentTurns+1 个轮次时不动历史、不调模型。
func TestCompactTooShort(t *testing.T) {
	fake := &fakeLLM{}
	a := New(fake, tools.NewRegistry(), "m", 64)
	a.Seed([]llm.Message{
		{Role: llm.RoleUser, Text: "only"},
		{Role: llm.RoleAssistant, Text: "turn"},
	})
	n, err := a.Compact(context.Background(), "")
	if err != nil || n != 0 {
		t.Fatalf("应 (0,nil)，got (%d,%v)", n, err)
	}
	if fake.calls != 0 {
		t.Fatalf("历史太短不应调模型，calls=%d", fake.calls)
	}
	if a.HistoryLen() != 2 {
		t.Fatalf("历史不应被改动")
	}
}

// TestCompactRefusedWhileRunning 验证：turn 进行中拒绝压缩。
func TestCompactRefusedWhileRunning(t *testing.T) {
	a := New(&fakeLLM{}, tools.NewRegistry(), "m", 64)
	seedThreeTurns(a)
	a.running.Store(true)
	defer a.running.Store(false)
	if _, err := a.Compact(context.Background(), ""); err == nil {
		t.Fatal("turn 进行中应拒绝压缩")
	}
}

// TestCompactPersistWatermark 验证：压缩本身不落盘（摘要不写文件），
// 之后的新 turn 只把新增消息交给 persist——水位线没被压缩搞炸。
func TestCompactPersistWatermark(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "摘要", StopReason: llm.StopEndTurn},
		{Text: "next reply", StopReason: llm.StopEndTurn},
	}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	seedThreeTurns(a)

	var batches [][]llm.Message
	a.SetPersist(func(ms []llm.Message) { batches = append(batches, ms) })

	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	a.flushPersist()
	if len(batches) != 0 {
		t.Fatalf("压缩不应触发落盘，got %d batches: %+v", len(batches), batches)
	}

	if err := a.Run(context.Background(), "turn4", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(batches) != 1 || len(batches[0]) != 2 ||
		batches[0][0].Text != "turn4" || batches[0][1].Text != "next reply" {
		t.Fatalf("压缩后新 turn 应只落 2 条新消息，got %+v", batches)
	}
}

// TestAutoCompactTriggers 验证：估算超阈值时 turn 开始前自动压缩，
// 经 OnNote 提示条数；本拍输入完整保留在压缩后的历史尾部。
func TestAutoCompactTriggers(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "自动摘要", StopReason: llm.StopEndTurn}, // 压缩调用
		{Text: "turn4 reply", StopReason: llm.StopEndTurn},
	}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	a.SetAutoCompact(10) // 远低于现有历史的估算，必触发
	seedThreeTurns(a)

	var note string
	err := a.Run(context.Background(), "turn4", UI{OnNote: func(s string) { note = s }})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(note, "已自动压缩 2 条历史") {
		t.Fatalf("OnNote 提示不对: %q", note)
	}
	msgs := a.Snapshot()
	// 摘要 + turn2(4) + turn3(2) + turn4 的 user/assistant = 9 条。
	if len(msgs) != 9 || !strings.HasPrefix(msgs[0].Text, SummaryPrefix) ||
		msgs[7].Text != "turn4" || msgs[8].Text != "turn4 reply" {
		t.Fatalf("自动压缩后历史形态不对（%d 条）: %+v", len(msgs), msgs)
	}
	if fake.calls != 2 {
		t.Fatalf("应 2 次模型调用（压缩+本拍），got %d", fake.calls)
	}
}

// TestAutoCompactDisabled 验证：阈值 0 时不触发压缩。
func TestAutoCompactDisabled(t *testing.T) {
	fake := &fakeLLM{responses: []llm.Response{{Text: "reply", StopReason: llm.StopEndTurn}}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	seedThreeTurns(a) // 不设阈值（零值=关闭）
	if err := a.Run(context.Background(), "turn4", UI{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("关闭时只应 1 次模型调用，got %d", fake.calls)
	}
}

// TestLastInputTokens 验证：runTurn 记住最近一次真实 input tokens。
func TestLastInputTokens(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // 非零 Usage 会触发记账，隔离到临时目录
	fake := &fakeLLM{responses: []llm.Response{
		{Text: "a", StopReason: llm.StopEndTurn, Usage: llm.Usage{InputTokens: 123}},
		{Text: "b", StopReason: llm.StopEndTurn}, // 端点未报：保留旧值
	}}
	a := New(fake, tools.NewRegistry(), "m", 64)
	if a.LastInputTokens() != 0 {
		t.Fatal("初始应为 0")
	}
	_ = a.Run(context.Background(), "x", UI{})
	if got := a.LastInputTokens(); got != 123 {
		t.Fatalf("应记住 123，got %d", got)
	}
	_ = a.Run(context.Background(), "y", UI{})
	if got := a.LastInputTokens(); got != 123 {
		t.Fatalf("零用量不应覆盖，got %d", got)
	}
}
