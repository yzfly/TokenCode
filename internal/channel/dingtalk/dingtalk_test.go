package dingtalk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/yzfly/tokencode/internal/channel"
)

// recvMsg 构造一条机器人回调消息（参数留空用合理默认）。
func recvMsg(msgID, convType, msgType, text string) *chatbot.BotCallbackDataModel {
	return &chatbot.BotCallbackDataModel{
		ConversationId:            "cid_1",
		ChatbotUserId:             "bot_1",
		MsgId:                     msgID,
		SenderNick:                "小明",
		SenderStaffId:             "staff_1",
		SenderId:                  "uid_enc",
		ConversationType:          convType,
		SessionWebhook:            "https://oapi.dingtalk.com/robot/sendBySession?session=abc",
		SessionWebhookExpiredTime: time.Now().Add(time.Hour).UnixMilli(),
		Text:                      chatbot.BotCallbackDataTextModel{Content: text},
		Msgtype:                   msgType,
	}
}

func TestAcceptParsesSingleChatText(t *testing.T) {
	a := New(Config{ClientID: "x", ClientSecret: "y"}, nil)
	now := time.Now()
	in, ok := a.accept(recvMsg("m1", "1", "text", " 修一下 bug\n顺便跑测试 "), now)
	if !ok {
		t.Fatal("合法单聊文本应通过")
	}
	want := channel.Inbound{
		Channel: "dingtalk", AccountID: "bot_1", UserID: "staff_1",
		UserName: "小明", ChatID: "cid_1", Text: "修一下 bug\n顺便跑测试",
	}
	if in != want {
		t.Fatalf("解析结果不对:\n got %+v\nwant %+v", in, want)
	}
}

func TestAcceptFallsBackToSenderId(t *testing.T) {
	a := New(Config{}, nil)
	m := recvMsg("m1", "1", "text", "hi")
	m.SenderStaffId = ""
	in, ok := a.accept(m, time.Now())
	if !ok || in.UserID != "uid_enc" {
		t.Fatalf("staffId 为空应兜底 senderId: ok=%v in=%+v", ok, in)
	}
}

func TestAcceptFilters(t *testing.T) {
	a := New(Config{}, nil)
	now := time.Now()
	noUser := recvMsg("m5", "1", "text", "hi")
	noUser.SenderStaffId, noUser.SenderId = "", ""
	cases := []struct {
		name string
		msg  *chatbot.BotCallbackDataModel
	}{
		{"群聊忽略", recvMsg("m1", "2", "text", "hi")},
		{"富媒体忽略", recvMsg("m2", "1", "picture", "")},
		{"空文本忽略", recvMsg("m3", "1", "text", "  \n ")},
		{"无发送者标识忽略", noUser},
		{"nil 忽略", nil},
	}
	for _, c := range cases {
		if _, ok := a.accept(c.msg, now); ok {
			t.Errorf("%s：不该通过", c.name)
		}
	}
}

func TestAcceptDedup(t *testing.T) {
	a := New(Config{}, nil)
	now := time.Now()
	if _, ok := a.accept(recvMsg("dup", "1", "text", "hi"), now); !ok {
		t.Fatal("首条应通过")
	}
	if _, ok := a.accept(recvMsg("dup", "1", "text", "hi"), now.Add(time.Second)); ok {
		t.Fatal("窗口内重复 msgId 应被丢弃")
	}
	// 窗口外同 id 重新接受（内存窗口语义）。
	if _, ok := a.accept(recvMsg("dup", "1", "text", "hi"), now.Add(dedupWindow+time.Minute)); !ok {
		t.Fatal("窗口外应重新接受")
	}
}

// fakeSender 捕获 Send 产物。
type fakeSender struct {
	webhook, text string
}

func (f *fakeSender) sendText(_ context.Context, webhook, text string) error {
	f.webhook, f.text = webhook, text
	return nil
}

func TestSendUsesRememberedWebhook(t *testing.T) {
	a := New(Config{}, nil)
	fs := &fakeSender{}
	a.sender = fs

	// 没收到过消息的会话：发不了。
	if err := a.Send(context.Background(), channel.Outbound{ChatID: "cid_1", Text: "hi"}); err == nil {
		t.Fatal("无 webhook 应报错")
	}

	// 入站后记下 webhook，Send 用它回。
	if _, ok := a.accept(recvMsg("m1", "1", "text", "hi"), time.Now()); !ok {
		t.Fatal("入站应通过")
	}
	if err := a.Send(context.Background(), channel.Outbound{ChatID: "cid_1", Text: "第一行\n\"引号\""}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(fs.webhook, "sendBySession") {
		t.Fatalf("webhook 不对: %q", fs.webhook)
	}
	if fs.text != "第一行\n\"引号\"" {
		t.Fatalf("text 不对: %q", fs.text)
	}
}

func TestSendExpiredWebhook(t *testing.T) {
	a := New(Config{}, nil)
	a.sender = &fakeSender{}
	m := recvMsg("m1", "1", "text", "hi")
	m.SessionWebhookExpiredTime = time.Now().Add(-time.Minute).UnixMilli()
	if _, ok := a.accept(m, time.Now()); !ok {
		t.Fatal("入站应通过")
	}
	err := a.Send(context.Background(), channel.Outbound{ChatID: "cid_1", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期 webhook 应报错: %v", err)
	}
}

func TestTextBodyEscapes(t *testing.T) {
	body, err := textBody("第一行\n\"引号\"与反斜杠\\")
	if err != nil {
		t.Fatalf("textBody: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"msgtype":"text"`) || !strings.Contains(s, `\n`) || !strings.Contains(s, `\"`) {
		t.Fatalf("消息体形态不对: %s", s)
	}
}

// TestWebhookSender 用本地 httptest 服务验证真发送实现：
// 消息体构造、HTTP 错误与 200+非零 errcode 都要能识别（不连真钉钉）。
func TestWebhookSender(t *testing.T) {
	var gotBody string
	reply := `{"errcode":0,"errmsg":"ok"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(reply))
	}))
	defer srv.Close()

	s := &webhookSender{cli: srv.Client()}
	if err := s.sendText(context.Background(), srv.URL, "你好\n换行"); err != nil {
		t.Fatalf("sendText: %v", err)
	}
	if !strings.Contains(gotBody, `"msgtype":"text"`) || !strings.Contains(gotBody, `你好\n换行`) {
		t.Fatalf("请求体不对: %s", gotBody)
	}

	// 钉钉特色：HTTP 200 但 errcode 非零（如频控）也是失败。
	reply = `{"errcode":130101,"errmsg":"send too fast"}`
	if err := s.sendText(context.Background(), srv.URL, "hi"); err == nil || !strings.Contains(err.Error(), "130101") {
		t.Fatalf("errcode 非零应报错: %v", err)
	}
}
