package wechat

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yzfly/tokencode/internal/channel"
)

// Config 是微信通道的装配配置。
type Config struct {
	// BaseURL 覆盖默认基座（协议灰度期防变动）；凭证里服务端下发的专属
	// baseurl 优先级更高（登录后要跟随）。
	BaseURL string
}

// dedupWindow 是消息去重窗口：msg_id 与内容 MD5 双键（iLink 长轮询游标
// 回绕/重放时两道闸都要有，Hermes 的事故经验）。
const dedupWindow = 5 * time.Minute

// Adapter 实现 channel.Adapter。启动时加载全部已登录账号，每账号一个
// getupdates 长轮询 goroutine；DM-only——peer 即会话（ChatID==UserID）。
type Adapter struct {
	cfg   Config
	store *Store
	logf  func(string, ...any)
	httpc *http.Client // 测试注入；nil 用默认

	// 时间参数集中放一处，测试调小。
	pollGap      time.Duration // 两次长轮询之间的喘息（防热循环）
	retryDelay   time.Duration // 一般失败重试间隔
	backoffDelay time.Duration // 连续失败/限频的退避
	sessionPause time.Duration // 会话过期后的暂停（期间提示重新扫码）
	sendRetries  int           // Send 限频退避的重试次数

	mu          sync.Mutex
	accounts    map[string]*account  // account_id → 运行态
	peerAccount map[string]string    // peer → account_id（Send 路由；入站学习 + 持久化 token 预热）
	seen        map[string]time.Time // 去重键 → 首见时间
}

// account 是一个已登录账号的运行态。
type account struct {
	cred   Credential
	client *Client

	mu     sync.Mutex
	tokens map[string]string // peer → 最新 context_token（磁盘有副本）
}

// New 创建微信 adapter。store 为 nil 用默认目录；logf 可为 nil。
func New(cfg Config, store *Store, logf func(string, ...any)) *Adapter {
	if store == nil {
		store = NewStore("")
	}
	return &Adapter{
		cfg:          cfg,
		store:        store,
		logf:         logf,
		pollGap:      300 * time.Millisecond,
		retryDelay:   2 * time.Second,
		backoffDelay: 30 * time.Second,
		sessionPause: 10 * time.Minute,
		sendRetries:  2,
		accounts:     map[string]*account{},
		peerAccount:  map[string]string{},
		seen:         map[string]time.Time{},
	}
}

// Name 实现 channel.Adapter。
func (a *Adapter) Name() string { return "wechat" }

// Start 加载全部已登录账号并为每个起一个长轮询 goroutine，阻塞到 ctx 取消。
// 没有账号不算错——空转等待（提示先 login），与「配置开了通道但还没人扫码」兼容。
func (a *Adapter) Start(ctx context.Context, sink func(channel.Inbound)) error {
	creds, err := a.store.List()
	if err != nil {
		return fmt.Errorf("wechat: 读取已登录账号失败: %w", err)
	}
	if len(creds) == 0 {
		a.log("通道 wechat 启动：尚无已登录账号（tokencode wechat login 扫码接入）")
	}
	var wg sync.WaitGroup
	for _, cred := range creds {
		acc := a.addAccount(cred)
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.poll(ctx, acc, sink)
		}()
		a.log("通道 wechat 账号 %s 上线（长轮询）", cred.AccountID)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// addAccount 装配一个账号运行态：基座取舍（服务端下发 > 配置覆盖 > 默认）、
// context_token 预热（重启可回话）、Send 路由表预热。
func (a *Adapter) addAccount(cred Credential) *account {
	acc := &account{
		cred: cred,
		client: &Client{
			BaseURL:    cmpOr(cred.BaseURL, a.cfg.BaseURL, DefaultBaseURL),
			Token:      cred.BotToken,
			HTTPClient: a.httpc,
		},
		tokens: a.store.LoadTokens(cred.AccountID),
	}
	a.mu.Lock()
	a.accounts[cred.AccountID] = acc
	for peer := range acc.tokens {
		a.peerAccount[peer] = cred.AccountID
	}
	a.mu.Unlock()
	return acc
}

// poll 是单账号的长轮询主循环：游标续传、错误分级退避、消息过滤投递。
func (a *Adapter) poll(ctx context.Context, acc *account, sink func(channel.Inbound)) {
	id := acc.cred.AccountID
	cursor := a.store.LoadCursor(id)
	failures := 0
	for ctx.Err() == nil {
		up, err := acc.client.GetUpdates(ctx, cursor)
		// 游标先于错误处理推进：部分错误响应也可能带新游标。
		if up.Buf != "" && up.Buf != cursor {
			cursor = up.Buf
			if werr := a.store.SaveCursor(id, cursor); werr != nil {
				a.log("通道 wechat 账号 %s 游标落盘失败: %v", id, werr)
			}
		}
		switch e := err.(type) {
		case nil:
			failures = 0
			for _, m := range up.Msgs {
				if in, ok := a.accept(acc, m, time.Now()); ok {
					sink(in)
				}
			}
			a.sleep(ctx, a.pollGap)
		case *ErrSessionExpired:
			a.log("通道 wechat 账号 %s 会话已过期（%v）：暂停收信，请重新执行 tokencode wechat login", id, e)
			a.sleep(ctx, a.sessionPause)
			failures = 0
		case *ErrRateLimited:
			a.log("通道 wechat 账号 %s 被限频（-2），退避 %v", id, a.backoffDelay)
			a.sleep(ctx, a.backoffDelay)
		default:
			if ctx.Err() != nil {
				return
			}
			failures++
			d := a.retryDelay
			if failures >= 3 {
				d = a.backoffDelay
				failures = 0
			}
			a.log("通道 wechat 账号 %s 收信失败（重试中）: %v", id, err)
			a.sleep(ctx, d)
		}
	}
}

// accept 过滤一条入站消息：滤 bot 自身/回显、双重去重、只认文本；
// 顺手记下 context_token（内存 + 落盘）与 Send 路由。
func (a *Adapter) accept(acc *account, m Message, now time.Time) (channel.Inbound, bool) {
	id := acc.cred.AccountID
	from := strings.TrimSpace(m.FromUserID)
	if from == "" || from == id {
		return channel.Inbound{}, false
	}
	if m.MessageType == msgTypeBot || strings.HasSuffix(from, "@im.bot") {
		return channel.Inbound{}, false // bot 消息（含自己发的回显）不进路由
	}
	text := strings.TrimSpace(m.Text())
	if text == "" {
		return channel.Inbound{}, false // v0 只做文本，媒体后置
	}
	// 双重去重：msg_id 一道，内容指纹一道（游标回绕时 msg_id 可能变）。
	if m.MessageID != "" && a.duplicate("id:"+id+":"+m.MessageID, now) {
		return channel.Inbound{}, false
	}
	sum := md5.Sum([]byte(text))
	if a.duplicate("md5:"+id+":"+from+":"+hex.EncodeToString(sum[:]), now) {
		return channel.Inbound{}, false
	}
	// context_token 是回话票据：内存即取即用，落盘保重启。
	if t := strings.TrimSpace(m.ContextToken); t != "" {
		acc.mu.Lock()
		acc.tokens[from] = t
		acc.mu.Unlock()
		if err := a.store.SaveToken(id, from, t); err != nil {
			a.log("通道 wechat 账号 %s context_token 落盘失败: %v", id, err)
		}
	}
	a.mu.Lock()
	a.peerAccount[from] = id
	a.mu.Unlock()
	return channel.Inbound{
		Channel:   a.Name(),
		AccountID: id,
		UserID:    from,
		ChatID:    from, // DM-only：peer 即会话
		Text:      text,
	}, true
}

// Send 按 ChatID（=peer）回一条文本：路由到学到的账号、回带该 peer 最新
// context_token；会话过期摘掉 token 降级重发一次，限频退避重试。
func (a *Adapter) Send(ctx context.Context, out channel.Outbound) error {
	acc := a.accountFor(out.ChatID)
	if acc == nil {
		return fmt.Errorf("wechat: 找不到会话 %s 归属的账号（尚未收到过该 peer 的消息）", out.ChatID)
	}
	acc.mu.Lock()
	token := acc.tokens[out.ChatID]
	acc.mu.Unlock()

	clientID := "tokencode-" + randomHex(8)
	strippedToken := false
	for attempt := 0; ; attempt++ {
		_, err := acc.client.SendMessage(ctx, out.ChatID, out.Text, token, clientID)
		switch err.(type) {
		case nil:
			return nil
		case *ErrSessionExpired:
			// Hermes 经验：摘掉 context_token 无票降级重发一次，可救活
			// cron 式推送；再失败就是真过期。
			if token != "" && !strippedToken {
				strippedToken = true
				token = ""
				acc.mu.Lock()
				delete(acc.tokens, out.ChatID)
				acc.mu.Unlock()
				continue
			}
			return fmt.Errorf("wechat: 账号 %s 会话已过期，请重新执行 tokencode wechat login: %w", acc.cred.AccountID, err)
		case *ErrRateLimited:
			if attempt >= a.sendRetries {
				return err
			}
			a.log("通道 wechat 发送被限频，退避 %v 后重试（%d/%d）", a.backoffDelay, attempt+1, a.sendRetries)
			if !a.sleep(ctx, a.backoffDelay) {
				return ctx.Err()
			}
		default:
			return err
		}
	}
}

// accountFor 找 peer 归属的账号：先查路由表，单账号场景兜底直选。
func (a *Adapter) accountFor(peer string) *account {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.peerAccount[peer]; ok {
		if acc := a.accounts[id]; acc != nil {
			return acc
		}
	}
	if len(a.accounts) == 1 {
		for _, acc := range a.accounts {
			return acc
		}
	}
	return nil
}

// duplicate 报告去重键是否在窗口内已见过（顺手清理过期项）。
func (a *Adapter) duplicate(key string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.seen {
		if now.Sub(t) > dedupWindow {
			delete(a.seen, k)
		}
	}
	if _, ok := a.seen[key]; ok {
		return true
	}
	a.seen[key] = now
	return false
}

// sleep 可被 ctx 打断地睡 d；返回 false 表示 ctx 已取消。
func (a *Adapter) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (a *Adapter) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
