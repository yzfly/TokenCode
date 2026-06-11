package wechat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LoginOptions 控制扫码登录流程。零值字段用默认。
type LoginOptions struct {
	BaseURL      string               // 空用 DefaultBaseURL
	Out          io.Writer            // 进度输出（空=io.Discard）
	RenderQR     func(content string) // 终端渲染二维码（空=只打印 URL）
	HTTPClient   *http.Client         // 测试注入
	PollInterval time.Duration        // 默认 1s
	Timeout      time.Duration        // 整体超时，默认 480s
	MaxRefresh   int                  // 过期自动刷新上限，默认 3
}

// Login 走完整扫码登录流：取码 → 展示 → 每秒轮询 →（redirect 换 host /
// 过期自动刷新）→ confirmed 返回凭证。调用方负责落盘。
func Login(ctx context.Context, opt LoginOptions) (Credential, error) {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = time.Second
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 480 * time.Second
	}
	if opt.MaxRefresh <= 0 {
		opt.MaxRefresh = 3
	}
	home := cmpOr(opt.BaseURL, DefaultBaseURL) // 取码/刷新固定打基座
	cli := &Client{BaseURL: home, HTTPClient: opt.HTTPClient}

	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	qr, err := cli.GetBotQRCode(ctx)
	if err != nil {
		return Credential{}, fmt.Errorf("wechat: 取二维码失败: %w", err)
	}
	if err := qr.Err(); err != nil {
		return Credential{}, fmt.Errorf("wechat: 取二维码失败: %w", err)
	}
	if qr.QRCode == "" {
		return Credential{}, fmt.Errorf("wechat: 二维码响应缺 qrcode 字段")
	}
	showQR(opt, qr)

	// 轮询基座：scaned_but_redirect 后切到服务端指定的 host。
	pollBase := home
	refreshes := 0
	for {
		select {
		case <-ctx.Done():
			return Credential{}, fmt.Errorf("wechat: 扫码登录超时/取消: %w", ctx.Err())
		case <-time.After(opt.PollInterval):
		}

		st, err := (&Client{BaseURL: pollBase, HTTPClient: opt.HTTPClient}).GetQRCodeStatus(ctx, qr.QRCode)
		if err != nil {
			if ctx.Err() != nil {
				return Credential{}, fmt.Errorf("wechat: 扫码登录超时/取消: %w", ctx.Err())
			}
			fmt.Fprintf(opt.Out, "（轮询出错，重试中: %v）\n", err)
			continue
		}

		// confirmed 状态字段拼写社区有出入，bot_token 到手即视为确认。
		if st.Status == "confirmed" || st.BotToken != "" {
			if st.AccountID() == "" || st.BotToken == "" {
				return Credential{}, fmt.Errorf("wechat: 扫码已确认但凭证不完整（ilink_bot_id/bot_token 缺失）")
			}
			return Credential{
				AccountID: st.AccountID(),
				BotToken:  st.BotToken,
				BaseURL:   cmpOr(st.BaseURL, pollBase),
				UserID:    st.IlinkUserID,
			}, nil
		}
		switch st.Status {
		case "wait", "":
			fmt.Fprint(opt.Out, ".")
		case "scaned", "scanned": // 两种拼写都见过
			fmt.Fprintln(opt.Out, "\n已扫码，请在手机微信上确认…")
		case "scaned_but_redirect":
			if h := strings.TrimSpace(st.RedirectHost); h != "" {
				if strings.Contains(h, "://") {
					pollBase = h
				} else {
					pollBase = "https://" + h
				}
				fmt.Fprintf(opt.Out, "\n（服务端要求换节点：%s）\n", pollBase)
			}
		case "expired":
			refreshes++
			if refreshes > opt.MaxRefresh {
				return Credential{}, fmt.Errorf("wechat: 二维码连续过期 %d 次，请重新执行登录", refreshes-1)
			}
			fmt.Fprintf(opt.Out, "\n二维码已过期，自动刷新（%d/%d）…\n", refreshes, opt.MaxRefresh)
			nq, err := cli.GetBotQRCode(ctx)
			if err != nil || nq.Err() != nil || nq.QRCode == "" {
				return Credential{}, fmt.Errorf("wechat: 刷新二维码失败: %v", firstErr(err, nq.Err()))
			}
			qr = nq
			pollBase = home
			showQR(opt, qr)
		default:
			fmt.Fprintf(opt.Out, "\n（未知状态 %q，继续轮询）\n", st.Status)
		}
	}
}

// showQR 展示二维码：优先终端 ASCII 渲染，URL 永远打印兜底。
func showQR(opt LoginOptions, qr QRCode) {
	content := cmpOr(qr.ImgContent, qr.QRCode) // 手机要扫的是完整 URL，不是裸 key
	fmt.Fprintln(opt.Out, "\n请用微信扫描下方二维码（或在手机浏览器打开链接）：")
	fmt.Fprintln(opt.Out, content)
	if opt.RenderQR != nil {
		opt.RenderQR(content)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return fmt.Errorf("响应不完整")
}
