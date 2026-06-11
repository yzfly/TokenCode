// Package feishu 实现飞书通道 adapter：官方 SDK 的长连接（WebSocket）收事件、
// im/v1/messages 发文本。免公网 IP——这是选飞书做第一个通道的核心原因。
//
// 与 SDK 之间留了一层薄接口（dial / sender）：路由与解析逻辑可用 fake 单测，
// SDK 本体不强行测。三个工程要点（调研结论照办）：
//   - 飞书要求 3 秒内 ack，否则重推 → 事件回调里立即返回、异步投 sink；
//   - 重推 + 重连可能造成重复投递 → event_id 内存去重窗口；
//   - 只认单聊（p2p）文本消息，群聊/富媒体直接忽略（v0 边界）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/yzfly/tokencode/internal/channel"
)

// Config 是飞书自建应用的凭据。
type Config struct {
	AppID     string
	AppSecret string
}

// dedupWindow 是 event_id 去重窗口：窗口内同一事件只投递一次。
const dedupWindow = 10 * time.Minute

// Adapter 实现 channel.Adapter。
type Adapter struct {
	cfg  Config
	logf func(string, ...any)

	// dial 建立长连接并阻塞（默认 SDK ws client）；sender 发消息（默认 im v1）。
	// 两者在测试里换成 fake。
	dial   func(ctx context.Context, h *dispatcher.EventDispatcher) error
	sender interface {
		sendText(ctx context.Context, chatID, content string) error
	}

	mu   sync.Mutex
	seen map[string]time.Time // event_id → 首见时间
}

// New 创建飞书 adapter。logf 可为 nil。
func New(cfg Config, logf func(string, ...any)) *Adapter {
	a := &Adapter{cfg: cfg, logf: logf, seen: map[string]time.Time{}}
	a.dial = a.dialWS
	a.sender = &larkSender{cli: lark.NewClient(cfg.AppID, cfg.AppSecret)}
	return a
}

// Name 实现 channel.Adapter。
func (a *Adapter) Name() string { return "feishu" }

// Start 建立长连接并阻塞直到 ctx 取消。SDK 默认自动重连。
func (a *Adapter) Start(ctx context.Context, sink func(channel.Inbound)) error {
	h := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			// 这里必须立即返回（3 秒 ack 红线）：解析+去重是纯内存操作，
			// 真正的 turn 在 sink 侧的 goroutine 里跑。
			if in, ok := a.accept(ev, time.Now()); ok {
				sink(in)
			}
			return nil
		})
	return a.dial(ctx, h)
}

// Send 发一条文本消息（按 chat_id）。换行经 JSON 转义原样保留。
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) error {
	content, err := textContent(out.Text)
	if err != nil {
		return err
	}
	return a.sender.sendText(ctx, out.ChatID, content)
}

// accept 解析并过滤一条收消息事件：只认单聊文本、滤掉非用户消息、
// event_id 去重。返回 ok=false 表示丢弃。
func (a *Adapter) accept(ev *larkim.P2MessageReceiveV1, now time.Time) (channel.Inbound, bool) {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return channel.Inbound{}, false
	}
	m := ev.Event.Message
	if deref(m.ChatType) != "p2p" {
		return channel.Inbound{}, false // v0 只处理单聊
	}
	if deref(m.MessageType) != "text" {
		return channel.Inbound{}, false // 富媒体后置
	}
	s := ev.Event.Sender
	if s == nil || deref(s.SenderType) != "user" || s.SenderId == nil {
		return channel.Inbound{}, false // 机器人/应用消息（含自己发的）不进路由
	}
	eventID := ""
	if ev.EventV2Base != nil && ev.EventV2Base.Header != nil {
		eventID = ev.EventV2Base.Header.EventID
	}
	if eventID != "" && a.duplicate(eventID, now) {
		return channel.Inbound{}, false
	}
	text := parseText(deref(m.Content))
	if text == "" {
		return channel.Inbound{}, false
	}
	appID := ""
	if ev.EventV2Base != nil && ev.EventV2Base.Header != nil {
		appID = ev.EventV2Base.Header.AppID
	}
	return channel.Inbound{
		Channel:   a.Name(),
		AccountID: appID,
		UserID:    deref(s.SenderId.OpenId),
		ChatID:    deref(m.ChatId),
		Text:      text,
	}, true
}

// duplicate 报告 event_id 是否在去重窗口内已见过（顺手清理过期项）。
func (a *Adapter) duplicate(id string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.seen {
		if now.Sub(t) > dedupWindow {
			delete(a.seen, k)
		}
	}
	if _, ok := a.seen[id]; ok {
		return true
	}
	a.seen[id] = now
	return false
}

// parseText 解出文本消息 content（JSON 字符串 {"text":"..."}）里的正文。
func parseText(content string) string {
	var v struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &v); err != nil {
		return ""
	}
	return v.Text
}

// textContent 构造发文本消息的 content（JSON 编码自带转义，换行/引号安全）。
func textContent(text string) (string, error) {
	raw, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// dialWS 用 SDK 的 ws client 建立长连接（自动重连默认开启），阻塞直到 ctx 取消。
func (a *Adapter) dialWS(ctx context.Context, h *dispatcher.EventDispatcher) error {
	cli := larkws.NewClient(a.cfg.AppID, a.cfg.AppSecret,
		larkws.WithEventHandler(h),
		larkws.WithLogLevel(larkcore.LogLevelWarn),
		larkws.WithOnReady(func() { a.log("通道 feishu 已连接（长连接就绪）") }),
		larkws.WithOnReconnected(func() { a.log("通道 feishu 已重连") }),
	)
	return cli.Start(ctx)
}

// larkSender 是真发送实现：im/v1/messages 按 chat_id 发 text。
type larkSender struct {
	cli *lark.Client
}

func (s *larkSender) sendText(ctx context.Context, chatID, content string) error {
	resp, err := s.cli.Im.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeText).
			Content(content).
			Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: 发送失败 code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// deref 安全取指针字符串。
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
