package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/headless"
)

// AssembleFunc 按绑定装配一个常驻 agent（工具注册表 SetRoot 到该成员的
// workspace、白名单/yolo 按绑定、usage Source = "channel:<name>"），
// 返回 agent 与解析后的 model id。装配由 cmd 层注入：路由不依赖 config/catalog，
// 测试用 fake LLM 工厂即可覆盖全部路径。
type AssembleFunc func(b Binding) (*agent.Agent, string, error)

// Router 是通道体系的中枢：入站消息 → 查绑定 → 跑 turn → 回复；
// 未绑定的走配对流程。
type Router struct {
	store    *Store
	assemble AssembleFunc
	logf     func(format string, args ...any) // 可为 nil

	mu       sync.Mutex
	adapters map[string]Adapter
	sessions map[string]*session
}

// session 是一个「channel+user+chat」的常驻会话：agent 持内存历史
// （进程生命周期）。互斥保证同一会话同时只跑一个 turn——TryLock 失败
// 即回「稍候」，新消息不排队（v0 实现简单优先）。
type session struct {
	mu    sync.Mutex
	ag    *agent.Agent
	model string
}

// NewRouter 创建路由。logf 可为 nil（不打日志）。
func NewRouter(store *Store, assemble AssembleFunc, logf func(string, ...any)) *Router {
	return &Router{
		store:    store,
		assemble: assemble,
		logf:     logf,
		adapters: map[string]Adapter{},
		sessions: map[string]*session{},
	}
}

// Register 挂一个通道 adapter（Start 之前调用）。
func (r *Router) Register(a Adapter) {
	r.mu.Lock()
	r.adapters[a.Name()] = a
	r.mu.Unlock()
}

// Start 为每个 adapter 起一个 goroutine 维持长连接，立即返回；
// ctx 取消即全部停下。adapter 退出错误走日志（重连是 adapter 自己的事，
// 走到这里说明它彻底放弃了）。
func (r *Router) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.adapters {
		go func(a Adapter) {
			// sink 再裹一层 goroutine：turn 可能跑很久，绝不堵 adapter 的收线。
			err := a.Start(ctx, func(in Inbound) { go r.Handle(ctx, in) })
			if err != nil && ctx.Err() == nil {
				r.log("通道 %s 退出: %v", a.Name(), err)
			}
		}(a)
	}
}

// Handle 处理一条入站消息（同步跑完整流程，含回复）。adapter 的 sink
// 已在独立 goroutine 里调它；测试也直接调它。
func (r *Router) Handle(ctx context.Context, in Inbound) {
	ad := r.adapter(in.Channel)
	if ad == nil {
		r.log("通道 %s 未注册，丢弃消息", in.Channel)
		return
	}
	reply := func(text string) {
		if err := ad.Send(ctx, Outbound{ChatID: in.ChatID, Text: text}); err != nil {
			r.log("通道 %s 回复失败: %v", in.Channel, err)
		}
	}
	if strings.TrimSpace(in.Text) == "" {
		return
	}

	b, ok, err := r.store.Find(in.Channel, in.UserID)
	if err != nil {
		r.log("查绑定失败: %v", err)
		reply("内部错误：读取绑定失败，请联系管理员。")
		return
	}
	if !ok {
		r.handlePairing(in, reply)
		return
	}

	s, err := r.session(in, b)
	if err != nil {
		r.log("装配会话失败（%s/%s）: %v", in.Channel, in.UserID, err)
		reply("内部错误：工作空间装配失败（" + err.Error() + "），请联系管理员检查绑定配置。")
		return
	}
	if !s.mu.TryLock() {
		reply("⏳ 上一条还在处理中，请稍候再发。")
		return
	}
	defer s.mu.Unlock()

	reply("⏳ 收到，干活中…")
	res := headless.Run(ctx, s.ag, s.model, in.Text, nil)
	if res.IsError {
		reply("❌ 出错了：" + res.Result)
		return
	}
	text := strings.TrimSpace(res.Result)
	if text == "" {
		text = "（本轮没有文本输出，但工具调用已执行完毕。）"
	}
	reply(text)
}

// handlePairing 处理未绑定用户的消息：像配对码就尝试认领，否则回引导语。
func (r *Router) handlePairing(in Inbound, reply func(string)) {
	if LooksLikeCode(in.Text) {
		b, ok, err := r.store.Pair(in.Channel, in.UserID, in.UserName, in.Text)
		if err != nil {
			r.log("配对失败: %v", err)
			reply("内部错误：配对写入失败，请联系管理员。")
			return
		}
		if ok {
			r.log("配对成功：%s/%s → %s", b.Channel, b.UserID, b.Workspace)
			reply(welcomeText(b))
			return
		}
		reply("配对码无效或已过期，请向管理员重新要一个（tokencode team pair）。")
		return
	}
	reply("你还没有绑定工作空间。请发送 8 位配对码完成绑定（管理员用 `tokencode team pair -workspace <目录>` 生成）。")
}

// welcomeText 是配对成功的欢迎语。
func welcomeText(b Binding) string {
	tools := strings.Join(b.AllowedTools, ", ")
	if b.Yolo {
		tools = "全部（yolo）"
	} else if tools == "" {
		tools = strings.Join(headless.DefaultAllowed, ", ") + "（默认只读集）"
	}
	name := b.Name
	if name != "" {
		name = name + "，"
	}
	return fmt.Sprintf("✅ %s配对成功！\n工作空间：%s\n可用工具：%s\n直接发消息即可驱动你的 agent。", name, b.Workspace, tools)
}

// session 取（或创建）该消息对应的常驻会话。装配在锁内做：装配是纯本地
// 操作（不打网络），代价可忽略，换来创建路径无竞态。
func (r *Router) session(in Inbound, b Binding) (*session, error) {
	key := in.Channel + "\x00" + in.UserID + "\x00" + in.ChatID
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[key]; ok {
		return s, nil
	}
	ag, model, err := r.assemble(b)
	if err != nil {
		return nil, err
	}
	s := &session{ag: ag, model: model}
	r.sessions[key] = s
	return s, nil
}

// adapter 按名取已注册的 adapter。
func (r *Router) adapter(name string) Adapter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adapters[name]
}

func (r *Router) log(format string, args ...any) {
	if r.logf != nil {
		r.logf(format, args...)
	}
}
