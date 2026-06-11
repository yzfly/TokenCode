// 上下文压缩（/compact）与 token 估算（/context）：长会话生存能力。
// IM 通道会话天然更长，历史无限增长会先撞上下文窗口、再撞钱包——
// 压缩把旧历史折叠成一条结构化摘要，估算器决定何时该折叠。
package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/usage"
)

// SummaryPrefix 是压缩摘要消息的固定开头（识别用：渲染/再次压缩时可辨认）。
const SummaryPrefix = "[历史压缩摘要]"

// keepRecentTurns 是压缩时保留的最近完整 user-assistant 轮次数。
const keepRecentTurns = 2

// 估算常数：UTF-8 字节数 / 3.5 是中英折中（中文 ~3 字节/字、~1.5 字/token
// → 4.5 字节/token；英文 ~4 字符=4 字节/token），取 3.5 偏保守（宁可高估，
// 提前压缩好过撞上下文窗口）。每条消息与每个工具块再计常数开销（角色标记、
// JSON 包装等结构性 token）。
const (
	estBytesPerToken = 3.5
	estMsgOverhead   = 8
	estToolOverhead  = 12
)

// EstimateTokens 启发式估算一段历史的 token 数（无 tokenizer 依赖）。
// 工具调用的名字与 JSON 入参、工具结果正文都计入。
func EstimateTokens(msgs []llm.Message) int {
	bytes, overhead := 0, 0
	for _, m := range msgs {
		overhead += estMsgOverhead
		bytes += len(m.Text)
		for _, tu := range m.ToolUses {
			overhead += estToolOverhead
			bytes += len(tu.Name) + len(tu.Input)
		}
		for _, tr := range m.ToolResults {
			overhead += estToolOverhead
			bytes += len(tr.Content)
		}
	}
	if bytes == 0 && overhead == 0 {
		return 0
	}
	return overhead + int(math.Ceil(float64(bytes)/estBytesPerToken))
}

// EstimatedTokens 返回当前历史的估算 token 数（/context 与自动压缩判定共用）。
func (a *Agent) EstimatedTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return EstimateTokens(a.msgs)
}

// LastInputTokens 返回最近一次模型调用的真实 input tokens（端点未报则为 0）。
// 估算是先验、真值是后验，/context 两者都给。
func (a *Agent) LastInputTokens() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInput
}

// recordInputTokens 在 runTurn 拿到 Response.Usage 时记下真实 input tokens。
func (a *Agent) recordInputTokens(n int) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.lastInput = n
	a.mu.Unlock()
}

// SetAutoCompact 设置自动压缩阈值（估算 tokens 超过即在 turn 开始前压缩；
// ≤0 关闭）。装配时调用一次。
func (a *Agent) SetAutoCompact(threshold int) {
	a.mu.Lock()
	a.autoCompact = threshold
	a.mu.Unlock()
}

// AutoCompactThreshold 返回当前自动压缩阈值（/context 展示余量用）。
func (a *Agent) AutoCompactThreshold() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoCompact
}

// Compact 把除最近 keepRecentTurns 个完整轮次外的历史交给当前模型生成
// 结构化摘要，用一条 SummaryPrefix 开头的 user 消息替换被压缩段。
// instructions 非空时作为用户侧重点追加进摘要指令。
// 返回被压缩（折叠进摘要）的消息条数；历史太短无可压缩时返回 (0, nil)。
//
// 互斥：turn 进行中拒绝压缩——TUI 只在 idle 态发起，这里再用 running CAS
// 兜底（同时让心跳的 Busy 判定把压缩期间的拍跳过去）。
//
// 与持久化的取舍（v0）：压缩只影响本进程内存。JSONL 落盘保持 append-only
// 不改写，被压缩的旧消息仍在盘上；摘要不落盘（水位线直接钉到新历史末尾），
// 之后的新消息照常落盘。-continue/-resume 恢复时 Seed 的是盘上全量历史，
// 即回到未压缩形态——可接受：恢复后再压缩即可，文件永远是完整事实记录。
func (a *Agent) Compact(ctx context.Context, instructions string) (int, error) {
	if !a.running.CompareAndSwap(false, true) {
		return 0, errors.New("有 turn 正在执行，无法压缩，请稍后再试")
	}
	defer a.running.Store(false)
	return a.compactNow(ctx, instructions)
}

// compactNow 是压缩主体。调用方保证此刻没有并发的 turn 在写 msgs
// （Compact 经 CAS 持锁；自动压缩在 turn 执行者 goroutine 上）。
func (a *Agent) compactNow(ctx context.Context, instructions string) (int, error) {
	msgs := a.Snapshot()
	cut := compactCut(msgs)
	if cut <= 0 {
		return 0, nil // 历史太短，无可压缩
	}
	old, kept := msgs[:cut], msgs[cut:]

	// 摘要请求：被压缩段 + 一条压缩指令，不带工具。old 以某轮的收尾
	// assistant 消息结束（cut 落在轮次开头），追加 user 指令合法。
	ask := "请把以上对话压缩为结构化摘要。"
	if strings.TrimSpace(instructions) != "" {
		ask += "\n用户要求额外侧重：" + strings.TrimSpace(instructions)
	}
	req := llm.Request{
		Model:     "",
		System:    compactSystem,
		Messages:  append(append([]llm.Message{}, old...), llm.Message{Role: llm.RoleUser, Text: ask}),
		MaxTokens: a.maxTokens,
	}
	client, model := a.client()
	req.Model = model
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("压缩失败（历史未动）: %w", err)
	}
	usage.Log(usage.Record{
		Model:      model,
		Source:     "compact",
		In:         resp.Usage.InputTokens,
		Out:        resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadTokens,
		CacheWrite: resp.Usage.CacheWriteTokens,
	})
	summary := llm.Message{
		Role: llm.RoleUser,
		Text: SummaryPrefix + " 以下是更早对话的压缩摘要，请视作既有上下文继续工作。\n\n" +
			strings.TrimSpace(resp.Text),
	}

	// 重建内存历史：摘要 + 保留轮次。水位线钉到新末尾——摘要与保留消息都
	// 不再（重复）落盘，flushPersist 的回落保护对此也安全（见上方取舍注释）。
	a.mu.Lock()
	a.msgs = append(a.msgs[:0], summary)
	a.msgs = append(a.msgs, kept...)
	a.persisted = len(a.msgs)
	a.mu.Unlock()
	return cut, nil
}

// autoCompactIfNeeded 在 turn 开始前检查估算用量，超阈值则就地压缩。
// 失败不阻塞本 turn（压缩是优化不是前置条件），经 OnNote 告知用户。
func (a *Agent) autoCompactIfNeeded(ctx context.Context, ui UI) {
	threshold := a.AutoCompactThreshold()
	if threshold <= 0 || a.EstimatedTokens() <= threshold {
		return
	}
	n, err := a.compactNow(ctx, "")
	if ui.OnNote == nil {
		return
	}
	switch {
	case err != nil:
		ui.OnNote("自动压缩失败（本拍照常进行）：" + err.Error())
	case n > 0:
		ui.OnNote(fmt.Sprintf("已自动压缩 %d 条历史（估算超过阈值 %d tokens）", n, threshold))
	}
}

// compactCut 算出压缩切点：保留最近 keepRecentTurns 个完整轮次（轮次开头 =
// 不带工具结果的 user 消息），返回被压缩段的长度。不足以压缩时返回 0。
func compactCut(msgs []llm.Message) int {
	var openers []int
	for i, m := range msgs {
		if m.Role == llm.RoleUser && len(m.ToolResults) == 0 {
			openers = append(openers, i)
		}
	}
	if len(openers) <= keepRecentTurns {
		return 0
	}
	return openers[len(openers)-keepRecentTurns]
}

// compactSystem 是摘要生成的系统提示：强调保留续作所需的硬信息。
const compactSystem = `你是对话历史压缩器。把给出的 agent 对话压缩成一份结构化摘要，供同一个 agent 在后续轮次中作为既有上下文继续工作。

必须保留（按节组织）：
- 任务目标：用户最初与最新的诉求
- 关键决定：已做的设计/实现决定及理由
- 文件与命令：涉及的文件路径、关键命令与其结果要点
- 未完成事项：still-open 的问题、下一步计划

丢弃寒暄、重复内容与失败后已被纠正的中间尝试。直接输出摘要正文，不要任何前言或解释。`
