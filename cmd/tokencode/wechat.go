package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	qrc "github.com/skip2/go-qrcode"

	"github.com/yzfly/tokencode/internal/channel/wechat"
)

// cmdWechat 实现 `tokencode wechat`：微信 iLink Bot 账号管理（实验性）。
// login 扫码接入、list 列已登录账号、logout 移除。通道本体在 serve 里跑。
func cmdWechat(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法：tokencode wechat login | list | logout <account_id>")
		return 2
	}
	switch args[0] {
	case "login":
		return cmdWechatLogin(args[1:])
	case "list":
		return cmdWechatList()
	case "logout":
		if len(args) < 2 {
			fmt.Println("用法：tokencode wechat logout <account_id>")
			return 2
		}
		store := wechat.NewStore("")
		if err := store.Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("已移除：", args[1])
		return 0
	default:
		fmt.Println("用法：tokencode wechat login | list | logout <account_id>")
		return 2
	}
}

// cmdWechatLogin 走完整扫码流并落盘凭证。
func cmdWechatLogin(args []string) int {
	fs := flag.NewFlagSet("wechat login", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "覆盖 iLink 基座（默认 "+wechat.DefaultBaseURL+"）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("微信 iLink Bot 登录（实验性）：扫码连上的是独立 bot 身份，不是你的个人号本身。")
	cred, err := wechat.Login(ctx, wechat.LoginOptions{
		BaseURL: *baseURL,
		Out:     os.Stdout,
		RenderQR: func(content string) {
			// 终端 ASCII 渲染；失败不致命（上面已打印可点开的 URL 兜底）。
			code, err := qrc.New(content, qrc.Low)
			if err != nil {
				fmt.Printf("（终端二维码渲染失败：%v，请打开上面的链接扫码）\n", err)
				return
			}
			fmt.Print(code.ToSmallString(false))
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	store := wechat.NewStore("")
	if err := store.SaveCredential(cred); err != nil {
		fmt.Fprintln(os.Stderr, "error: 凭证落盘失败:", err)
		return 1
	}
	fmt.Printf("\n✓ 登录成功！account_id=%s\n", cred.AccountID)
	fmt.Printf("凭证已保存到 %s（0600）\n\n", store.Dir())
	fmt.Println("下一步：")
	fmt.Println("  1. 在 config.json 的 channels 里开启微信通道：\"wechat\": {\"enabled\": true}")
	fmt.Println("  2. 管理员生成配对码：tokencode team pair -workspace <你的工作空间目录>")
	fmt.Println("  3. tokencode serve 启动后，用微信私聊这个 bot 发送配对码完成绑定")
	fmt.Println("注意：iLink bot 是 DM-only——队友要私聊该 bot 账号，而不是你本人。")
	return 0
}

// cmdWechatList 列出全部已登录账号。
func cmdWechatList() int {
	store := wechat.NewStore("")
	creds, err := store.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(creds) == 0 {
		fmt.Println("尚无已登录账号。tokencode wechat login 扫码接入。")
		return 0
	}
	fmt.Printf("已登录微信 bot 账号（%s）\n", store.Dir())
	for _, c := range creds {
		extra := ""
		if c.SavedAt != "" {
			extra = " · " + c.SavedAt
		}
		fmt.Printf("  %s%s\n", c.AccountID, extra)
	}
	return 0
}
