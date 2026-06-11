// Package llm 是 TokenCode 的 LLM provider 抽象。
// 内部用 Anthropic Messages API 的语义（content block + tool_use/tool_result + stop_reason）。
// 一个协议一个 codec，模块按协议命名：anthropic（默认指向 DeepSeek 的 /anthropic 端点）、
// openai（Chat Completions，DeepSeek/Kimi/Qwen/Ollama/OpenRouter 等通用）、
// google（Gemini generateContent）。
package llm

import (
	"context"
	"encoding/json"
)

// 消息角色。
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Tool 是暴露给模型的工具定义。
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema
}

// ToolUse 是模型发起的一次工具调用。
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult 是一次工具调用的结果，回灌给模型。
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// Message 是一条对话消息（内部表示）。
// assistant 消息可带 Text + ToolUses；user 消息可带 Text 或 ToolResults。
type Message struct {
	Role        string
	Text        string
	ToolUses    []ToolUse
	ToolResults []ToolResult
}

// Request 是一次补全请求。
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// StopReason 是模型停止输出的原因（归一化枚举，未知值落 StopOther）。
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	StopOther     StopReason = "other"
)

// Usage 是一次请求的 token 用量。零值表示该 codec/端点未提供对应数据。
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int // 命中提示词缓存读到的 token
	CacheWriteTokens int // 写入提示词缓存的 token（仅 anthropic 协议提供）
}

// Response 是模型一次回复。StopReason 为 StopToolUse 时表示模型要继续调工具。
// Thinking 是模型的推理内容，只读——回灌历史时只拷 Text/ToolUses，天然剥离。
type Response struct {
	Text       string
	ToolUses   []ToolUse
	StopReason StopReason
	Usage      Usage
	Thinking   string
}

// LLM 是 provider 抽象。
type LLM interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Delta 是流式生成过程中的一段增量。
type Delta struct {
	Text     string // 正文增量
	Thinking string // 推理增量
}

// Streamer 是 codec 的可选流式能力。实现者在生成过程中逐段回调 onDelta
// （onDelta 可为 nil，表示调用方只要最终结果），最终返回与 Complete 语义
// 完全相同的 Response——调用方（agent 循环）对两条路径零差别。
type Streamer interface {
	CompleteStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error)
}
