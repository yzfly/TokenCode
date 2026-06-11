package wechat

// 全部单测打 httptest 假 iLink 服务，绝不触真腾讯端点。覆盖：扫码全流程
// （redirect 换 host、过期刷新）、游标续传、context_token 回带与持久化、
// 双重去重、限频退避、会话过期暂停、Inbound 投递形态。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/channel"
)

// jsonOut 写一个 JSON 响应。
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody 解出 POST 体。
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("解请求体失败: %v", err)
	}
	return m
}

// textMsg 构造一条入站文本消息。
func textMsg(from, msgID, token, text string) map[string]any {
	return map[string]any{
		"from_user_id":  from,
		"message_id":    msgID,
		"message_type":  1,
		"context_token": token,
		"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]string{"text": text}}},
	}
}

// newTestAdapter 造一个时间参数全调小的 adapter。
func newTestAdapter(t *testing.T, cfg Config, store *Store) *Adapter {
	t.Helper()
	a := New(cfg, store, t.Logf)
	a.pollGap = time.Millisecond
	a.retryDelay = time.Millisecond
	a.backoffDelay = 5 * time.Millisecond
	a.sessionPause = 500 * time.Millisecond
	return a
}

// TestLoginFullFlow 走 wait → scaned → scaned_but_redirect（换到第二台
// 假服务器）→ confirmed 的完整扫码流，验证凭证与 host 跟随。
func TestLoginFullFlow(t *testing.T) {
	// 节点 B：redirect 之后的轮询都应打到这里，直接 confirmed。
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/get_qrcode_status" {
			t.Errorf("节点 B 收到意外请求 %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("qrcode"); got != "QR-KEY" {
			t.Errorf("节点 B qrcode = %q, want QR-KEY", got)
		}
		jsonOut(w, map[string]any{
			"status": "confirmed", "ilink_bot_id": "bot1@im.bot",
			"bot_token": "TOK", "ilink_user_id": "u1",
		})
	}))
	defer srvB.Close()

	// 节点 A（基座）：取码 + 前三次轮询。
	var polls atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if got := r.URL.Query().Get("bot_type"); got != "3" {
				t.Errorf("bot_type = %q, want 3", got)
			}
			jsonOut(w, map[string]any{"qrcode": "QR-KEY", "qrcode_img_content": "https://weixin.qq.com/x/QR-KEY"})
		case "/ilink/bot/get_qrcode_status":
			switch polls.Add(1) {
			case 1:
				jsonOut(w, map[string]any{"status": "wait"})
			case 2:
				jsonOut(w, map[string]any{"status": "scaned"})
			default:
				// redirect_host 宽容解析：带 scheme 原样用（测试服务器是 http）。
				jsonOut(w, map[string]any{"status": "scaned_but_redirect", "redirect_host": srvB.URL})
			}
		default:
			t.Errorf("节点 A 收到意外请求 %s", r.URL.Path)
		}
	}))
	defer srvA.Close()

	var out strings.Builder
	rendered := ""
	cred, err := Login(context.Background(), LoginOptions{
		BaseURL:      srvA.URL,
		Out:          &out,
		RenderQR:     func(c string) { rendered = c },
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if cred.AccountID != "bot1@im.bot" || cred.BotToken != "TOK" || cred.UserID != "u1" {
		t.Errorf("凭证不对: %+v", cred)
	}
	if cred.BaseURL != srvB.URL {
		t.Errorf("BaseURL 应跟随 redirect 节点 %s, got %s", srvB.URL, cred.BaseURL)
	}
	if rendered != "https://weixin.qq.com/x/QR-KEY" {
		t.Errorf("终端渲染内容应是完整 URL, got %q", rendered)
	}
	if !strings.Contains(out.String(), "已扫码") {
		t.Errorf("输出缺扫码提示: %q", out.String())
	}
}

// TestLoginExpiredRefresh 验证二维码过期自动刷新（≤3 次），刷新后用新码轮询。
func TestLoginExpiredRefresh(t *testing.T) {
	var qrIssued atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			n := qrIssued.Add(1)
			jsonOut(w, map[string]any{"qrcode": fmt.Sprintf("QR-%d", n), "qrcode_img_content": fmt.Sprintf("https://x/QR-%d", n)})
		case "/ilink/bot/get_qrcode_status":
			if r.URL.Query().Get("qrcode") == "QR-1" {
				jsonOut(w, map[string]any{"status": "expired"})
				return
			}
			jsonOut(w, map[string]any{"status": "confirmed", "ilink_bot_id": "b@im.bot", "bot_token": "T2", "baseurl": ""})
		}
	}))
	defer srv.Close()

	cred, err := Login(context.Background(), LoginOptions{
		BaseURL: srv.URL, PollInterval: time.Millisecond, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if cred.BotToken != "T2" || qrIssued.Load() != 2 {
		t.Errorf("应刷新一次二维码后登录成功: cred=%+v issued=%d", cred, qrIssued.Load())
	}
	if cred.BaseURL != srv.URL {
		t.Errorf("服务端没下发 baseurl 时应回落到轮询基座, got %q", cred.BaseURL)
	}
}

// TestLoginExpiredGivesUp 验证连续过期超过上限后放弃。
func TestLoginExpiredGivesUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			jsonOut(w, map[string]any{"qrcode": "QR"})
		case "/ilink/bot/get_qrcode_status":
			jsonOut(w, map[string]any{"status": "expired"})
		}
	}))
	defer srv.Close()

	_, err := Login(context.Background(), LoginOptions{
		BaseURL: srv.URL, PollInterval: time.Millisecond, Timeout: 5 * time.Second, MaxRefresh: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("应报连续过期错误, got %v", err)
	}
}

// fakeUpdates 是一个可编排的 getupdates/sendmessage 假服务。
type fakeUpdates struct {
	t *testing.T

	mu       sync.Mutex
	bufsSeen []string         // 每次 getupdates 收到的游标
	rounds   []map[string]any // 依次吐出的响应（耗尽后回空响应）
	sends    []map[string]any // 每次 sendmessage 的 msg 体
	sendResp []map[string]any // 依次吐出的发送响应（耗尽后回 ret 0）
}

func (f *fakeUpdates) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			// 顺手校验工程头（每个 POST 都该带）。
			if r.Header.Get("AuthorizationType") != "ilink_bot_token" ||
				!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
				r.Header.Get("X-WECHAT-UIN") == "" || r.Header.Get("iLink-App-Id") != "bot" {
				f.t.Errorf("请求头不齐: %v", r.Header)
			}
			body := decodeBody(f.t, r)
			f.mu.Lock()
			f.bufsSeen = append(f.bufsSeen, fmt.Sprint(body["get_updates_buf"]))
			var resp map[string]any
			if len(f.rounds) > 0 {
				resp = f.rounds[0]
				f.rounds = f.rounds[1:]
			} else {
				resp = map[string]any{"ret": 0, "msgs": []any{}}
			}
			f.mu.Unlock()
			jsonOut(w, resp)
		case "/ilink/bot/sendmessage":
			body := decodeBody(f.t, r)
			msg, _ := body["msg"].(map[string]any)
			f.mu.Lock()
			f.sends = append(f.sends, msg)
			var resp map[string]any
			if len(f.sendResp) > 0 {
				resp = f.sendResp[0]
				f.sendResp = f.sendResp[1:]
			} else {
				resp = map[string]any{"ret": 0}
			}
			f.mu.Unlock()
			jsonOut(w, resp)
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *fakeUpdates) sendCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sends) }
func (f *fakeUpdates) pollCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bufsSeen) }

// startAdapter 起 adapter 并返回 Inbound chan 与停止函数。
func startAdapter(t *testing.T, a *Adapter) (<-chan channel.Inbound, func()) {
	t.Helper()
	ch := make(chan channel.Inbound, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.Start(ctx, func(in channel.Inbound) { ch <- in }); err != nil && ctx.Err() == nil {
			t.Errorf("Start: %v", err)
		}
	}()
	return ch, func() { cancel(); <-done }
}

func waitInbound(t *testing.T, ch <-chan channel.Inbound) channel.Inbound {
	t.Helper()
	select {
	case in := <-ch:
		return in
	case <-time.After(3 * time.Second):
		t.Fatal("等 Inbound 超时")
		return channel.Inbound{}
	}
}

// seedCred 在 store 里放一个已登录账号。
func seedCred(t *testing.T, store *Store, baseURL string) Credential {
	t.Helper()
	cred := Credential{AccountID: "bot1@im.bot", BotToken: "TOK", BaseURL: baseURL, UserID: "u1"}
	if err := store.SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	return cred
}

// TestAdapterInboundAndCursorResume 验证 Inbound 投递形态、游标落盘、
// 重启后续传（第一拉带上次游标）。
func TestAdapterInboundAndCursorResume(t *testing.T) {
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{textMsg("alice", "m1", "CT1", "你好")}},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	cred := seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	in := waitInbound(t, ch)
	stop()

	want := channel.Inbound{Channel: "wechat", AccountID: cred.AccountID, UserID: "alice", ChatID: "alice", Text: "你好"}
	if in != want {
		t.Errorf("Inbound = %+v, want %+v", in, want)
	}
	if got := store.LoadCursor(cred.AccountID); got != "B1" {
		t.Errorf("游标应落盘 B1, got %q", got)
	}
	f.mu.Lock()
	if f.bufsSeen[0] != "" {
		t.Errorf("首拉游标应为空, got %q", f.bufsSeen[0])
	}
	f.mu.Unlock()

	// 重启：新 adapter 实例第一拉就要带 B1。
	a2 := newTestAdapter(t, Config{}, store)
	_, stop2 := startAdapter(t, a2)
	deadline := time.Now().Add(3 * time.Second)
	for f.pollCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop2()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bufsSeen) < 2 || f.bufsSeen[1] != "B1" {
		t.Errorf("重启后首拉应续传游标 B1, bufs=%v", f.bufsSeen)
	}
}

// TestAdapterDedup 验证双重去重：同 msg_id 重放、不同 msg_id 同内容（5 分钟窗）
// 都只投递一次；不同内容正常放行。
func TestAdapterDedup(t *testing.T) {
	dup := textMsg("alice", "m1", "", "重复的话")
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{dup}},
		{"ret": 0, "get_updates_buf": "B2", "msgs": []any{dup}},                                // msg_id 重放
		{"ret": 0, "get_updates_buf": "B3", "msgs": []any{textMsg("alice", "m2", "", "重复的话")}}, // 内容指纹重放
		{"ret": 0, "get_updates_buf": "B4", "msgs": []any{textMsg("alice", "m3", "", "新的话")}},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	defer stop()

	if in := waitInbound(t, ch); in.Text != "重复的话" {
		t.Errorf("第一条 = %q", in.Text)
	}
	if in := waitInbound(t, ch); in.Text != "新的话" {
		t.Errorf("去重后下一条应是「新的话」, got %q", in.Text)
	}
	select {
	case in := <-ch:
		t.Errorf("不应再有投递, got %+v", in)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestAdapterFiltersBotMessages 验证 bot 自身/回显消息与非文本消息不进路由。
func TestAdapterFiltersBotMessages(t *testing.T) {
	echo := textMsg("bot1@im.bot", "m1", "", "我自己")
	echo["message_type"] = 2
	other := textMsg("peer@im.bot", "m2", "", "别的 bot")
	empty := map[string]any{"from_user_id": "alice", "message_id": "m3", "item_list": []any{map[string]any{"type": 2}}}
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{echo, other, empty, textMsg("alice", "m4", "", "真人")}},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	defer stop()
	if in := waitInbound(t, ch); in.Text != "真人" || in.UserID != "alice" {
		t.Errorf("只应投递真人消息, got %+v", in)
	}
}

// TestSendEchoesContextToken 验证回话回带该 peer 最新 context_token，
// 且 token 持久化——新 adapter 实例（重启）仍能带对 token 回话。
func TestSendEchoesContextToken(t *testing.T) {
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{textMsg("alice", "m1", "CT-OLD", "一")}},
		{"ret": 0, "get_updates_buf": "B2", "msgs": []any{textMsg("alice", "m2", "CT-NEW", "二")}},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	cred := seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	waitInbound(t, ch)
	waitInbound(t, ch)
	if err := a.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "回个话"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stop()

	f.mu.Lock()
	if len(f.sends) != 1 {
		t.Fatalf("应发出 1 条, got %d", len(f.sends))
	}
	msg := f.sends[0]
	f.mu.Unlock()
	if msg["context_token"] != "CT-NEW" {
		t.Errorf("应回带最新 context_token CT-NEW, got %v", msg["context_token"])
	}
	if msg["to_user_id"] != "alice" || msg["message_type"] != float64(2) || msg["message_state"] != float64(2) {
		t.Errorf("发送报文不对: %v", msg)
	}
	itemList, _ := msg["item_list"].([]any)
	if len(itemList) != 1 {
		t.Fatalf("item_list 不对: %v", msg["item_list"])
	}

	// 重启可回话：新实例不收任何消息直接 Send，token 从盘上来。
	a2 := newTestAdapter(t, Config{}, store)
	a2.addAccount(cred)
	if err := a2.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "重启后再回"}); err != nil {
		t.Fatalf("重启后 Send: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.sends[1]["context_token"]; got != "CT-NEW" {
		t.Errorf("重启后应仍回带 CT-NEW, got %v", got)
	}
}

// TestSendRateLimitBackoff 验证发送遇 -2 限频退避后重试成功；重试耗尽则报错。
func TestSendRateLimitBackoff(t *testing.T) {
	f := &fakeUpdates{t: t, sendResp: []map[string]any{
		{"ret": -2, "errmsg": "freq limit"},
		{"ret": 0},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	cred := seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	a.addAccount(cred)
	if err := a.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "hi"}); err != nil {
		t.Fatalf("限频后重试应成功: %v", err)
	}
	if f.sendCount() != 2 {
		t.Errorf("应发 2 次（1 退避重试）, got %d", f.sendCount())
	}

	// 一直 -2：重试耗尽报 ErrRateLimited。
	f.mu.Lock()
	f.sendResp = []map[string]any{{"ret": -2}, {"ret": -2}, {"ret": -2}, {"ret": -2}}
	f.mu.Unlock()
	err := a.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "again"})
	if _, ok := err.(*ErrRateLimited); !ok {
		t.Fatalf("应报 ErrRateLimited, got %v", err)
	}
}

// TestSendSessionExpiredFallback 验证 -14 时摘 token 降级重发一次；
// 仍失败则报「重新扫码」级错误。
func TestSendSessionExpiredFallback(t *testing.T) {
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{textMsg("alice", "m1", "CT1", "一")}},
	}, sendResp: []map[string]any{
		{"errcode": -14, "errmsg": "session expired"},
		{"ret": 0},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	defer stop()
	waitInbound(t, ch)

	if err := a.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "hi"}); err != nil {
		t.Fatalf("降级重发应成功: %v", err)
	}
	f.mu.Lock()
	if len(f.sends) != 2 {
		t.Fatalf("应发 2 次, got %d", len(f.sends))
	}
	if _, has := f.sends[0]["context_token"]; !has {
		t.Errorf("第一发应带 token")
	}
	if _, has := f.sends[1]["context_token"]; has {
		t.Errorf("降级重发不应带 token: %v", f.sends[1])
	}
	f.sendResp = []map[string]any{{"errcode": -14}, {"errcode": -14}}
	f.mu.Unlock()

	err := a.Send(context.Background(), channel.Outbound{ChatID: "alice", Text: "again"})
	if err == nil || !strings.Contains(err.Error(), "wechat login") {
		t.Fatalf("应提示重新扫码, got %v", err)
	}
}

// TestPollSessionExpiredPauses 验证收信遇 -14：暂停轮询并打「重新扫码」提示。
func TestPollSessionExpiredPauses(t *testing.T) {
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"errcode": -14, "errmsg": "session expired"},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	seedCred(t, store, srv.URL)

	var mu sync.Mutex
	var logs []string
	a := New(Config{}, store, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	a.pollGap = time.Millisecond
	a.sessionPause = 500 * time.Millisecond

	_, stop := startAdapter(t, a)
	defer stop()
	time.Sleep(250 * time.Millisecond) // 暂停窗口的一半
	if n := f.pollCount(); n != 1 {
		t.Errorf("-14 后应暂停轮询, 期内只应有 1 次拉取, got %d", n)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "wechat login") {
		t.Errorf("应提示重新扫码, logs=%q", joined)
	}
}

// TestPollRateLimitBackoff 验证收信遇 -2 退避后继续轮询（不放弃账号）。
func TestPollRateLimitBackoff(t *testing.T) {
	f := &fakeUpdates{t: t, rounds: []map[string]any{
		{"ret": -2, "errmsg": "freq limit"},
		{"ret": 0, "get_updates_buf": "B1", "msgs": []any{textMsg("alice", "m1", "", "退避后到达")}},
	}}
	srv := f.server()
	defer srv.Close()
	store := NewStore(t.TempDir())
	seedCred(t, store, srv.URL)

	a := newTestAdapter(t, Config{}, store)
	ch, stop := startAdapter(t, a)
	defer stop()
	if in := waitInbound(t, ch); in.Text != "退避后到达" {
		t.Errorf("限频退避后应继续收信, got %+v", in)
	}
}

// TestSendUnknownPeer 验证没学到归属账号且多账号并存时拒发。
func TestSendUnknownPeer(t *testing.T) {
	store := NewStore(t.TempDir())
	a := newTestAdapter(t, Config{}, store)
	a.addAccount(Credential{AccountID: "b1@im.bot", BotToken: "T1", BaseURL: "http://127.0.0.1:1"})
	a.addAccount(Credential{AccountID: "b2@im.bot", BotToken: "T2", BaseURL: "http://127.0.0.1:1"})
	if err := a.Send(context.Background(), channel.Outbound{ChatID: "stranger", Text: "hi"}); err == nil {
		t.Fatal("多账号下未知 peer 应拒发")
	}
}

// TestStoreCredentialLifecycle 验证凭证 0600 落盘、list、logout 连带清状态。
func TestStoreCredentialLifecycle(t *testing.T) {
	store := NewStore(t.TempDir())
	cred := Credential{AccountID: "b1@im.bot", BotToken: "T1", BaseURL: "http://x"}
	if err := store.SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if err := store.SaveCursor(cred.AccountID, "B9"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if err := store.SaveToken(cred.AccountID, "alice", "CT"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	info, err := os.Stat(store.credPath(cred.AccountID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("凭证权限 = %o, want 600", perm)
	}

	creds, err := store.List()
	if err != nil || len(creds) != 1 || creds[0].AccountID != "b1@im.bot" {
		t.Fatalf("List = %+v, %v", creds, err)
	}
	if creds[0].SavedAt == "" {
		t.Error("SavedAt 应自动补")
	}

	if err := store.Remove(cred.AccountID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if creds, _ := store.List(); len(creds) != 0 {
		t.Errorf("logout 后应无账号: %+v", creds)
	}
	if store.LoadCursor(cred.AccountID) != "" || len(store.LoadTokens(cred.AccountID)) != 0 {
		t.Error("logout 应连带清掉游标与 token")
	}
	if err := store.Remove("ghost"); err == nil {
		t.Error("移除不存在的账号应报错")
	}
}
