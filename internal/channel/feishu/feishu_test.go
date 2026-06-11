package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/yzfly/tokencode/internal/channel"
)

func ptr(s string) *string { return &s }

// recvEvent 构造一条收消息事件（参数留空用合理默认）。
func recvEvent(eventID, chatType, msgType, senderType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{
			Header: &larkevent.EventHeader{EventID: eventID, AppID: "cli_app"},
		},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: ptr(senderType),
				SenderId:   &larkim.UserId{OpenId: ptr("ou_user")},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_1"),
				ChatId:      ptr("oc_chat"),
				ChatType:    ptr(chatType),
				MessageType: ptr(msgType),
				Content:     ptr(content),
			},
		},
	}
}

func TestAcceptParsesP2PText(t *testing.T) {
	a := New(Config{AppID: "x", AppSecret: "y"}, nil)
	now := time.Now()
	in, ok := a.accept(recvEvent("ev1", "p2p", "text", "user", `{"text":"修一下 bug\n顺便跑测试"}`), now)
	if !ok {
		t.Fatal("合法单聊文本应通过")
	}
	want := channel.Inbound{
		Channel: "feishu", AccountID: "cli_app", UserID: "ou_user",
		ChatID: "oc_chat", Text: "修一下 bug\n顺便跑测试",
	}
	if in != want {
		t.Fatalf("解析结果不对:\n got %+v\nwant %+v", in, want)
	}
}

func TestAcceptFilters(t *testing.T) {
	a := New(Config{}, nil)
	now := time.Now()
	cases := []struct {
		name string
		ev   *larkim.P2MessageReceiveV1
	}{
		{"群聊忽略", recvEvent("e1", "group", "text", "user", `{"text":"hi"}`)},
		{"富媒体忽略", recvEvent("e2", "p2p", "image", "user", `{"image_key":"k"}`)},
		{"应用消息忽略", recvEvent("e3", "p2p", "text", "app", `{"text":"hi"}`)},
		{"content 损坏忽略", recvEvent("e4", "p2p", "text", "user", `not-json`)},
		{"空事件忽略", &larkim.P2MessageReceiveV1{}},
		{"nil 忽略", nil},
	}
	for _, c := range cases {
		if _, ok := a.accept(c.ev, now); ok {
			t.Errorf("%s：不该通过", c.name)
		}
	}
}

func TestAcceptDedup(t *testing.T) {
	a := New(Config{}, nil)
	now := time.Now()
	if _, ok := a.accept(recvEvent("dup", "p2p", "text", "user", `{"text":"hi"}`), now); !ok {
		t.Fatal("首条应通过")
	}
	if _, ok := a.accept(recvEvent("dup", "p2p", "text", "user", `{"text":"hi"}`), now.Add(time.Second)); ok {
		t.Fatal("窗口内重复 event_id 应被丢弃")
	}
	// 窗口外同 id 重新接受（内存窗口语义）。
	if _, ok := a.accept(recvEvent("dup", "p2p", "text", "user", `{"text":"hi"}`), now.Add(dedupWindow+time.Minute)); !ok {
		t.Fatal("窗口外应重新接受")
	}
}

// fakeSender 捕获 Send 产物，验证 content 构造与转义。
type fakeSender struct {
	chatID, content string
}

func (f *fakeSender) sendText(_ context.Context, chatID, content string) error {
	f.chatID, f.content = chatID, content
	return nil
}

func TestSendBuildsEscapedContent(t *testing.T) {
	a := New(Config{}, nil)
	fs := &fakeSender{}
	a.sender = fs
	err := a.Send(context.Background(), channel.Outbound{ChatID: "oc_1", Text: "第一行\n\"引号\"与反斜杠\\"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if fs.chatID != "oc_1" {
		t.Fatalf("chatID = %q", fs.chatID)
	}
	if got := parseText(fs.content); got != "第一行\n\"引号\"与反斜杠\\" {
		t.Fatalf("content 转义回环失败: %q → %q", fs.content, got)
	}
	if !strings.HasPrefix(fs.content, `{"text":`) {
		t.Fatalf("content 形态不对: %q", fs.content)
	}
}
