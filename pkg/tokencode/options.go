package tokencode

// options 是 New 的装配参数集合，只经 Option 函数填充。
type options struct {
	model       string
	client      LLM    // WithLLM 注入；非 nil 时跳过 config 解析
	clientModel string // WithLLM 给定的 model id
	tools       []Tool
	allowed     []string // nil=未配置（全放行）；非 nil（含空表）=启用白名单守卫
	root        string
	maxTokens   int
	system      string
	usageSrc    string
}

// Option 配置 New 的一项装配参数。
type Option func(*options)

// WithModel 指定模型：models 别名、"provider/model-id"（用户 config 或
// 内置目录），或直传给默认端点的 model id。空串等价于不设——走默认链
// （ANTHROPIC_MODEL → config default_model → 内置默认）。
func WithModel(name string) Option {
	return func(o *options) { o.model = name }
}

// WithLLM 直接注入模型客户端（自定义实现、测试 fake、或绕过 config 的
// 现成 codec），model 是记入请求与用量记账的 model id。设置后 WithModel 失效。
func WithLLM(client LLM, model string) Option {
	return func(o *options) {
		o.client = client
		o.clientModel = model
	}
}

// WithTools 指定 agent 可用的工具集（通常从 DefaultTools 起步）。
// 不设则没有任何工具——agent 退化为纯对话。
func WithTools(ts []Tool) Option {
	return func(o *options) { o.tools = append(o.tools, ts...) }
}

// WithAllowedTools 启用工具白名单：名单之外的调用不执行，以错误 tool_result
// 喂回模型（与 headless 的 -allowed-tools 同语义）。缺省不设防——SDK 用户
// 对宿主环境自己负责。
func WithAllowedTools(names ...string) Option {
	return func(o *options) {
		if o.allowed == nil {
			o.allowed = []string{}
		}
		o.allowed = append(o.allowed, names...)
	}
}

// WithRoot 绑定工具根：文件工具的相对路径基于它解析且只允许访问它之内
// （符号链接逃逸也会被拦截），bash 在它之下执行。空=不限制。
func WithRoot(dir string) Option {
	return func(o *options) { o.root = dir }
}

// WithMaxTokens 设置单次模型调用的输出 token 上限（默认 DefaultMaxTokens）。
func WithMaxTokens(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxTokens = n
		}
	}
}

// WithSystemPrompt 覆盖默认系统提示（整体替换，不是追加）。
func WithSystemPrompt(s string) Option {
	return func(o *options) { o.system = s }
}

// WithUsageSource 设置用量记账的来源标签（默认 "sdk"），用于在
// `tokencode usage` 里区分宿主应用。
func WithUsageSource(s string) Option {
	return func(o *options) {
		if s != "" {
			o.usageSrc = s
		}
	}
}
