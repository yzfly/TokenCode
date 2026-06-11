// Package tokencode 是 TokenCode 的 Go SDK 门面：十行代码起一个 agent。
//
// 它对 internal 各包（agent / headless / tools / llm / config）只包一层稳定
// API，不搬动任何实现——执行与事件语义与 `tokencode -p` 完全一致。
//
// 最小用法：
//
//	tc, _ := tokencode.New(
//	    tokencode.WithModel("kimi-for-coding/k2p6"),
//	    tokencode.WithTools(tokencode.DefaultTools()),
//	)
//	out, _ := tc.Run(ctx, "fix the bug")
//
// 历史在 Client 内累积，连续 Run 即多轮对话；RunStream 给出与
// `tokencode -p -output stream-json` 同构的事件流。
package tokencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/headless"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// 核心类型经 type alias 原样导出：第三方实现自定义 LLM / Tool 时只依赖
// 本包，不需要（也不能）import internal。alias 不是新类型，与内部实现
// 天然互通，是门面里最薄的一层。
type (
	// Tool 是 agent 可调用的一个能力（internal/tools.Tool 的别名）。
	Tool = tools.Tool
	// LLM 是模型客户端抽象（internal/llm.LLM 的别名），配合 WithLLM 注入自定义实现。
	LLM = llm.LLM
	// Streamer 是 LLM 的可选流式能力（internal/llm.Streamer 的别名）。
	Streamer = llm.Streamer
	// Request 是一次补全请求（internal/llm.Request 的别名）。
	Request = llm.Request
	// Response 是模型一次回复（internal/llm.Response 的别名）。
	Response = llm.Response
	// Message 是一条对话消息（internal/llm.Message 的别名），History 返回它。
	Message = llm.Message
	// ToolUse 是模型发起的一次工具调用（internal/llm.ToolUse 的别名）。
	ToolUse = llm.ToolUse
	// ToolResult 是一次工具调用的结果（internal/llm.ToolResult 的别名）。
	ToolResult = llm.ToolResult
	// Usage 是一次请求的 token 用量（internal/llm.Usage 的别名）。
	Usage = llm.Usage
	// Delta 是流式生成的一段增量（internal/llm.Delta 的别名）。
	Delta = llm.Delta
	// Event 是 RunStream 的一条事件（internal/headless.Event 的别名）。
	// 流的最后一条恒为 Type=="result"。
	Event = headless.Event
)

// 消息角色（History 返回的 Message.Role 取值）。
const (
	RoleUser      = llm.RoleUser
	RoleAssistant = llm.RoleAssistant
)

// DefaultMaxTokens 是未经 WithMaxTokens 配置时的输出上限，与 CLI 默认一致。
const DefaultMaxTokens = 4096

// DefaultTools 返回内置工具全家桶：read / write / edit / bash / websearch / webfetch。
// 每次调用返回新实例，可安全用于多个 Client。
func DefaultTools() []Tool {
	return []Tool{
		tools.Read(), tools.Write(), tools.Edit(), tools.Bash(),
		tools.WebSearch(), tools.WebFetch(),
	}
}

// Client 是一个可对话的 agent 实例。历史在其中累积；非并发安全——
// 同一 Client 上的 Run / RunStream 已内部串行化，但事件回调里不要再调它们。
type Client struct {
	mu    sync.Mutex // 串行化 Run/RunStream：agent 历史只允许单写者
	ag    *agent.Agent
	reg   *tools.Registry
	gate  func(Tool) Tool
	model string
}

// New 装配一个 agent：模型解析（config + 内置目录）→ 协议客户端 →
// 工具注册表（可选白名单守卫与根隔离）。零 Option 也能用：默认链为
// ANTHROPIC_MODEL → config 的 default_model → 内置默认模型。
func New(opts ...Option) (*Client, error) {
	o := options{maxTokens: DefaultMaxTokens, usageSrc: "sdk"}
	for _, fn := range opts {
		fn(&o)
	}

	client, model := o.client, o.clientModel
	if client == nil {
		var err error
		if client, model, err = resolveClient(o.model); err != nil {
			return nil, err
		}
	}

	// 白名单缺省不设防（SDK 用户自己负责）；一旦显式给出，所有工具
	// （含此后 AddTool 的）都包上 headless 同款守卫，拒绝以 tool_result
	// 错误喂回模型而非中断执行。
	gate := func(t Tool) Tool { return t }
	if o.allowed != nil {
		allow := headless.Allow(o.allowed, false)
		gate = func(t Tool) Tool { return headless.GateTool(t, allow) }
	}
	reg := tools.NewRegistry()
	for _, t := range o.tools {
		reg.Add(gate(t))
	}
	if o.root != "" {
		reg.SetRoot(o.root)
	}

	ag := agent.New(client, reg, model, o.maxTokens)
	if o.system != "" {
		ag.SetSystem(o.system)
	}
	ag.SetUsageSource(o.usageSrc)
	return &Client{ag: ag, reg: reg, gate: gate, model: model}, nil
}

// Run 跑一个 turn（可能多次调用模型与工具）并返回最终 assistant 文本。
// 历史留在 Client 里，连续调用即多轮对话。
func (c *Client) Run(ctx context.Context, prompt string) (string, error) {
	return c.run(ctx, prompt, nil)
}

// RunStream 跑一个 turn 并随执行过程回调事件（assistant_delta / tool_call /
// tool_result，最后一条恒为 result）。回调被串行化，并行工具下也安全。
func (c *Client) RunStream(ctx context.Context, prompt string, onEvent func(Event)) error {
	if onEvent == nil {
		return errors.New("tokencode: RunStream 需要非 nil 的事件回调（不要事件请用 Run）")
	}
	_, err := c.run(ctx, prompt, onEvent)
	return err
}

func (c *Client) run(ctx context.Context, prompt string, onEvent func(Event)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res := headless.Run(ctx, c.ag, c.model, prompt, onEvent)
	if res.IsError {
		return "", errors.New(res.Result)
	}
	return res.Result, nil
}

// AddTool 注册一个自定义工具（重名覆盖）。WithAllowedTools 配置过白名单时，
// 新工具同样受其约束。
func (c *Client) AddTool(t Tool) {
	c.reg.Add(c.gate(t))
}

// History 返回到目前为止的对话历史（值拷贝，元素只读）。
func (c *Client) History() []Message {
	return c.ag.Snapshot()
}

// Reset 清空对话历史，Client 回到刚 New 完的状态（工具与模型不变）。
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ag.Seed(nil)
}

// Model 返回实际生效的 model id（经 config / 内置目录解析后的落点）。
func (c *Client) Model() string {
	return c.model
}

// resolveClient 按名字解析并构造协议客户端：名字为空走默认链
// （ANTHROPIC_MODEL → config default_model → 内置默认）。缺凭据时
// 原样透传 config 的错误，其中带 `tokencode auth login` 指引。
func resolveClient(name string) (LLM, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	if name == "" {
		name = config.EnvModel()
	}
	if name == "" {
		name = cfg.DefaultModel
	}
	if name == "" {
		name = llm.DefaultModel
	}
	tgt, err := cfg.Resolve(name)
	if err != nil {
		return nil, "", err
	}
	client, err := buildClient(tgt)
	if err != nil {
		return nil, "", err
	}
	return client, tgt.Model, nil
}

// buildClient 按解析落点构造客户端，与 CLI 的装配逻辑同构
// （cmd 包不可被 import，此处是门面自己的极薄复刻）。
func buildClient(tgt config.Target) (LLM, error) {
	if tgt.Default {
		burl := config.EnvBaseURL()
		if burl == "" {
			burl = llm.DefaultBaseURL
		}
		if key, bearer, ok := config.EnvAuth(); ok {
			return llm.NewAnthropic(key, burl, bearer), nil
		}
		return nil, errors.New("tokencode: 缺少凭据——设置 TOKENCODE_AUTH_TOKEN（或 ANTHROPIC_AUTH_TOKEN / *_API_KEY），" +
			"或用 WithModel 指定内置目录里的 provider（运行 `tokencode auth login <provider>` 存 key），" +
			"或用 WithLLM 直接注入客户端")
	}
	switch tgt.Protocol {
	case config.ProtocolAnthropic:
		burl := tgt.BaseURL
		if burl == "" {
			burl = llm.DefaultBaseURL
		}
		return llm.NewAnthropic(tgt.APIKey, burl, tgt.Bearer), nil
	case config.ProtocolOpenAI:
		if tgt.BaseURL == "" {
			return nil, errors.New("tokencode: provider 缺少 base_url（openai 协议必填）")
		}
		return llm.NewOpenAI(tgt.APIKey, tgt.BaseURL), nil
	case config.ProtocolGoogle:
		burl := tgt.BaseURL
		if burl == "" {
			burl = llm.DefaultGoogleBaseURL
		}
		return llm.NewGoogle(tgt.APIKey, burl), nil
	default:
		return nil, fmt.Errorf("tokencode: 未知协议 %q", tgt.Protocol)
	}
}
