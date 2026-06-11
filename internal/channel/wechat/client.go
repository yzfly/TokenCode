// Package wechat 实现微信 iLink Bot API 通道（实验性）：腾讯 2026 年开放的
// 官方 bot 协议（ClawBot 背后的 HTTP/JSON 接口），形态类似 Telegram Bot API——
// QR 登录 + getupdates 长轮询 + sendmessage，纯 Go、免浏览器/Windows 宿主。
//
// 关键认知：扫码连上的是独立 iLink bot 身份（xxx@im.bot），不是个人号本身，
// 实际可靠的只有私聊 DM。团队成员各自扫码 = 各自一个 account。
//
// 协议无正式公开文档，字段名以 Hermes weixin.py / wechat-ilink-demo 源码为准，
// 解析从宽（两家拼写有差异的字段全都认）。
package wechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL 是 iLink 官方基座。登录后服务端可能下发专属 baseurl，要跟随。
const DefaultBaseURL = "https://ilinkai.weixin.qq.com"

// 协议常量（与 Hermes weixin.py 对齐）。
const (
	channelVersion = "2.2.0"
	appClientVer   = 2<<16 | 2<<8 // (2,2,0) 打包成一个 int

	itemText     = 1 // item_list 里的文本项
	msgTypeBot   = 2 // bot 自己发出的消息（收侧要滤掉）
	msgStateDone = 2 // FINISH

	errcodeRateLimit      = -2  // 限频：退避重试
	errcodeSessionExpired = -14 // 会话过期：暂停并提示重新扫码
)

// 各请求超时（长轮询服务端 35s 持有，客户端给足余量）。
const (
	longPollTimeout = 40 * time.Second
	apiTimeout      = 15 * time.Second
)

// Client 是 iLink Bot API 的最小 HTTP 客户端。Token 为空时只能调扫码两个 GET。
type Client struct {
	BaseURL    string       // 空用 DefaultBaseURL
	Token      string       // bot_token
	HTTPClient *http.Client // 空用 http.DefaultClient
}

// ErrRateLimited 表示 iLink 返回限频（errcode -2），调用方应退避。
type ErrRateLimited struct{ Msg string }

func (e *ErrRateLimited) Error() string { return "ilink: 限频（-2）: " + e.Msg }

// ErrSessionExpired 表示会话过期（errcode -14，或 -2 + "unknown error" 的
// 陈旧会话伪装），需要重新扫码。
type ErrSessionExpired struct{ Msg string }

func (e *ErrSessionExpired) Error() string { return "ilink: 会话过期（-14）: " + e.Msg }

// respStatus 是所有响应共有的状态字段。
type respStatus struct {
	Ret     int    `json:"ret"`
	Errcode int    `json:"errcode"`
	Errmsg  string `json:"errmsg"`
}

// Err 把状态字段翻译成 error（nil 表示成功）。陈旧会话判定照搬 Hermes：
// -2 且 errmsg=="unknown error" 实为会话过期而非限频。
func (r respStatus) Err() error {
	if r.Ret == 0 && r.Errcode == 0 {
		return nil
	}
	stale := (r.Ret == errcodeRateLimit || r.Errcode == errcodeRateLimit) &&
		strings.EqualFold(strings.TrimSpace(r.Errmsg), "unknown error")
	if r.Ret == errcodeSessionExpired || r.Errcode == errcodeSessionExpired || stale {
		return &ErrSessionExpired{Msg: r.Errmsg}
	}
	if r.Ret == errcodeRateLimit || r.Errcode == errcodeRateLimit {
		return &ErrRateLimited{Msg: r.Errmsg}
	}
	return fmt.Errorf("ilink: ret=%d errcode=%d errmsg=%s", r.Ret, r.Errcode, r.Errmsg)
}

// QRCode 是 get_bot_qrcode 的响应。
type QRCode struct {
	respStatus
	QRCode     string `json:"qrcode"`             // 轮询用的 key
	ImgContent string `json:"qrcode_img_content"` // 完整可扫 URL（终端渲染/兜底打印用这个）
}

// QRStatus 是 get_qrcode_status 的响应。confirmed 时凭证字段才有值。
type QRStatus struct {
	respStatus
	Status       string `json:"status"` // wait / scaned(scanned) / scaned_but_redirect / confirmed / expired
	RedirectHost string `json:"redirect_host"`
	IlinkBotID   string `json:"ilink_bot_id"` // Hermes 用名
	BotID        string `json:"bot_id"`       // demo 用名（宽容解析，两个都认）
	BotToken     string `json:"bot_token"`
	BaseURL      string `json:"baseurl"`
	IlinkUserID  string `json:"ilink_user_id"`
}

// AccountID 返回账号标识（两种拼写取其有）。
func (s QRStatus) AccountID() string {
	if s.IlinkBotID != "" {
		return s.IlinkBotID
	}
	return s.BotID
}

// Updates 是 getupdates 的响应。
type Updates struct {
	respStatus
	Buf  string    `json:"get_updates_buf"` // 游标，必须落盘续传
	Msgs []Message `json:"msgs"`
}

// Message 是一条入站消息（只解 v0 需要的字段）。
type Message struct {
	FromUserID   string        `json:"from_user_id"`
	ToUserID     string        `json:"to_user_id"`
	MessageID    string        `json:"message_id"`
	MessageType  int           `json:"message_type"`
	ContextToken string        `json:"context_token"` // 回话必须回带的票据，按 peer 持久化
	ItemList     []MessageItem `json:"item_list"`
}

// MessageItem 是消息体里的一项。v0 只认文本。
type MessageItem struct {
	Type     int `json:"type"`
	TextItem *struct {
		Text string `json:"text"`
	} `json:"text_item"`
}

// Text 取出消息的文本正文（第一个文本项）；非文本消息返回空。
func (m Message) Text() string {
	for _, it := range m.ItemList {
		if it.Type == itemText && it.TextItem != nil {
			return it.TextItem.Text
		}
	}
	return ""
}

// SendResult 是 sendmessage 的响应。
type SendResult struct {
	respStatus
}

// GetBotQRCode 取登录二维码。
func (c *Client) GetBotQRCode(ctx context.Context) (QRCode, error) {
	var out QRCode
	err := c.get(ctx, "/ilink/bot/get_bot_qrcode?bot_type=3", &out)
	return out, err
}

// GetQRCodeStatus 轮询二维码状态。
func (c *Client) GetQRCodeStatus(ctx context.Context, qrcode string) (QRStatus, error) {
	var out QRStatus
	err := c.get(ctx, "/ilink/bot/get_qrcode_status?qrcode="+url.QueryEscape(qrcode), &out)
	return out, err
}

// GetUpdates 长轮询收消息（服务端最长持有 35s）。buf 是上次返回的游标。
// 返回的状态错误（限频/过期）已翻译进 error，但响应体仍返回（游标可能有效）。
func (c *Client) GetUpdates(ctx context.Context, buf string) (Updates, error) {
	var out Updates
	body := map[string]any{"get_updates_buf": buf, "base_info": baseInfo()}
	if err := c.post(ctx, "/ilink/bot/getupdates", body, &out, longPollTimeout); err != nil {
		return out, err
	}
	return out, out.Err()
}

// SendMessage 发一条文本 DM。contextToken 须回带该 peer 最近一次入站的票据
// （空表示无票据降级发送）；clientID 是客户端幂等标识。
func (c *Client) SendMessage(ctx context.Context, to, text, contextToken, clientID string) (SendResult, error) {
	msg := map[string]any{
		"from_user_id":  "",
		"to_user_id":    to,
		"client_id":     clientID,
		"message_type":  msgTypeBot,
		"message_state": msgStateDone,
		"item_list":     []any{map[string]any{"type": itemText, "text_item": map[string]string{"text": text}}},
	}
	if contextToken != "" {
		msg["context_token"] = contextToken
	}
	var out SendResult
	body := map[string]any{"msg": msg, "base_info": baseInfo()}
	if err := c.post(ctx, "/ilink/bot/sendmessage", body, &out, apiTimeout); err != nil {
		return out, err
	}
	return out, out.Err()
}

// SendTyping 发「输入中」指示（可选增强，失败不致命）。status 1=typing 2=cancel。
func (c *Client) SendTyping(ctx context.Context, ilinkUserID, typingTicket string, status int) error {
	var out SendResult
	body := map[string]any{
		"ilink_user_id": ilinkUserID,
		"typing_ticket": typingTicket,
		"status":        status,
		"base_info":     baseInfo(),
	}
	if err := c.post(ctx, "/ilink/bot/sendtyping", body, &out, apiTimeout); err != nil {
		return err
	}
	return out.Err()
}

// get 发一个无鉴权 GET（扫码两端点用）。
func (c *Client) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", strconv.Itoa(appClientVer))
	return c.do(req, out)
}

// post 发一个带 bot_token 鉴权的 POST。
func (c *Client) post(ctx context.Context, path string, body any, out any, timeout time.Duration) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	req.Header.Set("iLink-App-Id", "bot")
	req.Header.Set("iLink-App-ClientVersion", strconv.Itoa(appClientVer))
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.do(req, out)
}

// do 执行请求并解 JSON。HTTP 非 2xx 直接报错（带响应片段帮助排障）。
func (c *Client) do(req *http.Request, out any) error {
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ilink: %s HTTP %d: %s", req.URL.Path, resp.StatusCode, truncate(string(raw), 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ilink: %s 响应解析失败: %w", req.URL.Path, err)
	}
	return nil
}

func (c *Client) base() string { return strings.TrimRight(cmpOr(c.BaseURL, DefaultBaseURL), "/") }

// baseInfo 是每个 POST 都要带的版本信息。
func baseInfo() map[string]string { return map[string]string{"channel_version": channelVersion} }

// randomUIN 生成 X-WECHAT-UIN：随机 uint32 的十进制字符串再 base64。
func randomUIN() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	v := binary.BigEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(v), 10)))
}

// cmpOr 返回第一个非空字符串。
func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
