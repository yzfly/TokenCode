// Package dingtalk 实现钉钉通道 adapter：官方 Stream SDK 的长连接（WebSocket）
// 收机器人单聊消息、sessionWebhook 回文本。免公网 IP，与飞书 adapter 同构。
//
// 与 SDK 之间留了一层薄接口（dial / sender）：解析与去重逻辑可用 fake 单测，
// SDK 本体不强行测。工程要点（调研结论 + SDK 源码确认）：
//   - 回调返回即是 ack（SDK 把返回值包成 success 帧回给平台）→ 回调内纯内存
//     操作立即返回，真正的 turn 在 sink 侧 goroutine 里跑（同飞书 3 秒红线的姿势）；
//   - Stream 重推/重连可能重复投递 → msgId 内存去重窗口；
//   - 回复走 sessionWebhook（入站消息自带、官方回复原语，免 access token 管理）：
//     按会话缓存最近一次入站的 webhook，过期/没有就报错——Router 的回复永远
//     紧跟入站消息，语义天然吻合；
//   - 只认单聊（conversationType="1"）文本，群聊@与富媒体 v0 忽略（同飞书）；
//   - SDK 的 Start 是一次性的（连不上即报错返回）、连上后断线重连内置（无限重试
//     3 秒间隔）→ 初连重试在 dial 层自己外包循环；SDK 内部重连不认我们的 ctx，
//     ctx 取消 Close 后残留的重连 goroutine 可能短暂复活连接——serve 场景
//     ctx 取消即进程退出，可接受。
package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dtclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/yzfly/tokencode/internal/channel"
)

// Config 是钉钉企业内部应用（机器人）的凭据。
type Config struct {
	ClientID     string
	ClientSecret string
}

// dedupWindow 是 msgId 去重窗口：窗口内同一消息只投递一次。
const dedupWindow = 10 * time.Minute

// Adapter 实现 channel.Adapter。
type Adapter struct {
	cfg  Config
	logf func(string, ...any)

	// dial 建立长连接并阻塞（默认 SDK Stream client）；sender 发消息
	// （默认 POST sessionWebhook）。两者在测试里换成 fake。
	dial   func(ctx context.Context, h chatbot.IChatBotMessageHandler) error
	sender interface {
		sendText(ctx context.Context, webhook, text string) error
	}

	mu    sync.Mutex
	seen  map[string]time.Time   // msgId → 首见时间
	hooks map[string]sessionHook // conversationId → 最近一次入站的回复 webhook
}

// sessionHook 是一条会话的回复落点：入站消息携带的 sessionWebhook 及其有效期。
type sessionHook struct {
	url    string
	expire time.Time // 零值 = 平台没给有效期，视为不过期
}

// New 创建钉钉 adapter。logf 可为 nil。
func New(cfg Config, logf func(string, ...any)) *Adapter {
	a := &Adapter{
		cfg:   cfg,
		logf:  logf,
		seen:  map[string]time.Time{},
		hooks: map[string]sessionHook{},
	}
	a.dial = a.dialStream
	a.sender = &webhookSender{cli: &http.Client{Timeout: 15 * time.Second}}
	return a
}

// Name 实现 channel.Adapter。
func (a *Adapter) Name() string { return "dingtalk" }

// Start 建立长连接并阻塞直到 ctx 取消。
func (a *Adapter) Start(ctx context.Context, sink func(channel.Inbound)) error {
	return a.dial(ctx, func(_ context.Context, msg *chatbot.BotCallbackDataModel) ([]byte, error) {
		// 这里必须立即返回（返回即 ack）：解析+去重是纯内存操作，
		// 真正的 turn 在 sink 侧的 goroutine 里跑。
		if in, ok := a.accept(msg, time.Now()); ok {
			sink(in)
		}
		return []byte("{}"), nil
	})
}

// Send 发一条文本消息：查该会话缓存的 sessionWebhook 并 POST。
// 钉钉的回复原语只能回「发过消息的会话」——Router 的回复永远紧跟入站，吻合。
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) error {
	url, err := a.webhookFor(out.ChatID, time.Now())
	if err != nil {
		return err
	}
	return a.sender.sendText(ctx, url, out.Text)
}

// accept 解析并过滤一条机器人回调消息：只认单聊文本、msgId 去重，
// 顺手记下该会话的回复 webhook。返回 ok=false 表示丢弃。
func (a *Adapter) accept(msg *chatbot.BotCallbackDataModel, now time.Time) (channel.Inbound, bool) {
	if msg == nil {
		return channel.Inbound{}, false
	}
	if msg.ConversationType != "1" {
		return channel.Inbound{}, false // v0 只处理单聊（"2" 群聊@后置）
	}
	if msg.Msgtype != "text" {
		return channel.Inbound{}, false // 富媒体后置
	}
	userID := msg.SenderStaffId // 企业内员工的稳定工号
	if userID == "" {
		userID = msg.SenderId // 兜底：加密的钉钉用户标识，同样稳定
	}
	if userID == "" || msg.ConversationId == "" {
		return channel.Inbound{}, false
	}
	if msg.MsgId != "" && a.duplicate(msg.MsgId, now) {
		return channel.Inbound{}, false
	}
	text := strings.TrimSpace(msg.Text.Content)
	if text == "" {
		return channel.Inbound{}, false
	}
	a.rememberHook(msg)
	return channel.Inbound{
		Channel:   a.Name(),
		AccountID: msg.ChatbotUserId,
		UserID:    userID,
		UserName:  msg.SenderNick,
		ChatID:    msg.ConversationId,
		Text:      text,
	}, true
}

// duplicate 报告 msgId 是否在去重窗口内已见过（顺手清理过期项）。
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

// rememberHook 记下该会话最新的回复 webhook（新消息覆盖旧的，有效期一并刷新）。
func (a *Adapter) rememberHook(msg *chatbot.BotCallbackDataModel) {
	if msg.SessionWebhook == "" {
		return
	}
	h := sessionHook{url: msg.SessionWebhook}
	if msg.SessionWebhookExpiredTime > 0 {
		h.expire = time.UnixMilli(msg.SessionWebhookExpiredTime)
	}
	a.mu.Lock()
	a.hooks[msg.ConversationId] = h
	a.mu.Unlock()
}

// webhookFor 取该会话当前可用的回复 webhook。
func (a *Adapter) webhookFor(chatID string, now time.Time) (string, error) {
	a.mu.Lock()
	h, ok := a.hooks[chatID]
	a.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("dingtalk: 会话 %s 没有可用的回复 webhook（只能回复发过消息的会话）", chatID)
	}
	if !h.expire.IsZero() && now.After(h.expire) {
		return "", fmt.Errorf("dingtalk: 会话 %s 的回复 webhook 已过期（成员再发一条消息即恢复）", chatID)
	}
	return h.url, nil
}

// dialStream 用 SDK 的 Stream client 建立长连接，阻塞直到 ctx 取消。
// SDK 的 Start 一次性、连上后断线重连内置（无限重试）→ 初连重试在这层外包。
func (a *Adapter) dialStream(ctx context.Context, h chatbot.IChatBotMessageHandler) error {
	cli := dtclient.NewStreamClient(
		dtclient.WithAppCredential(dtclient.NewAppCredentialConfig(a.cfg.ClientID, a.cfg.ClientSecret)),
	)
	cli.RegisterChatBotCallbackRouter(h)
	for {
		err := cli.Start(ctx)
		if err == nil {
			break
		}
		a.log("通道 dingtalk 连接失败（3 秒后重试）: %v", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
	a.log("通道 dingtalk 已连接（Stream 就绪）")
	<-ctx.Done()
	cli.Close()
	return nil
}

// webhookSender 是真发送实现：POST sessionWebhook 发 text 消息。
type webhookSender struct {
	cli *http.Client
}

func (s *webhookSender) sendText(ctx context.Context, webhook, text string) error {
	body, err := textBody(text)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dingtalk: 发送失败 http=%d body=%s", resp.StatusCode, raw)
	}
	// 钉钉 webhook 失败也可能回 200 + 非零 errcode（如频控），必须解 body 确认。
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &r); err == nil && r.ErrCode != 0 {
		return fmt.Errorf("dingtalk: 发送失败 code=%d msg=%s", r.ErrCode, r.ErrMsg)
	}
	return nil
}

// textBody 构造 text 消息体（JSON 编码自带转义，换行/引号安全）。
func textBody(text string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"msgtype": "text",
		"text":    map[string]string{"content": text},
	})
}

func (a *Adapter) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}
