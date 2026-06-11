package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/channel"
	"github.com/yzfly/tokencode/internal/channel/dingtalk"
	"github.com/yzfly/tokencode/internal/channel/feishu"
	"github.com/yzfly/tokencode/internal/channel/wechat"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/headless"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/serve"
	"github.com/yzfly/tokencode/internal/webui"
)

// cmdServe 实现 `tokencode serve`：HTTP API 雏形。默认只绑回环（v0 无鉴权，
// 安全边界就是 127.0.0.1）；每个请求装配独立 agent，权限语义与 -p 完全一致。
func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "监听地址（v0 无鉴权，改绑非回环前想清楚）")
	maxConc := fs.Int("max-concurrent", 8, "同时在跑的 run 上限（超出排队）")
	model := fs.String("model", "", "默认模型（请求未带 model 字段时用）")
	maxTokens := fs.Int("max-tokens", 4096, "max output tokens")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	// 默认模型优先级与主命令一致：flag > env > config > 内置默认。
	def := *model
	if def == "" {
		def = config.EnvModel()
	}
	if def == "" {
		def = cfg.DefaultModel
	}
	if def == "" {
		def = llm.DefaultModel
	}

	srv := &serve.Server{
		Version:       version,
		MaxConcurrent: *maxConc,
		Assemble: func(model string, allowed []string) (*agent.Agent, string, error) {
			name := model
			if name == "" {
				name = def
			}
			return assembleHeadless(cfg, name, "", *maxTokens, allowed, false, "serve", "")
		},
		// WebUI（大盘/聊天/团队/模型）：与 IM 路由共用同一份 team.json。
		Mount: (&webui.Server{Version: version, Team: channel.NewStore("")}).Register,
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// IM 通道（团队模式）：config 配了 channels 才起。每个成员绑定一个
	// workspace，agent 常驻（内存历史），工具被硬隔离在该 workspace 之内。
	if cfg.Channels.Feishu.Enabled() || cfg.Channels.Dingtalk.Enabled() || cfg.Channels.Wechat.Enabled {
		logf := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
		router := channel.NewRouter(channel.NewStore(""), func(b channel.Binding) (*agent.Agent, string, error) {
			name := b.Model
			if name == "" {
				name = def
			}
			allowed := b.AllowedTools
			if len(allowed) == 0 {
				allowed = headless.DefaultAllowed
			}
			return assembleHeadless(cfg, name, "", *maxTokens, allowed, b.Yolo, "channel:"+b.Channel, b.Workspace)
		}, logf)
		if cfg.Channels.Feishu.Enabled() {
			router.Register(feishu.New(feishu.Config{AppID: cfg.Channels.Feishu.AppID, AppSecret: cfg.Channels.Feishu.AppSecret}, logf))
			fmt.Println("通道 feishu 启动中（长连接，免公网 IP）· 配对：tokencode team pair -workspace <目录>")
		}
		if cfg.Channels.Dingtalk.Enabled() {
			router.Register(dingtalk.New(dingtalk.Config{ClientID: cfg.Channels.Dingtalk.ClientID, ClientSecret: cfg.Channels.Dingtalk.ClientSecret}, logf))
			fmt.Println("通道 dingtalk 启动中（Stream 长连接，免公网 IP）· 配对：tokencode team pair -workspace <目录>")
		}
		if cfg.Channels.Wechat.Enabled {
			router.Register(wechat.New(wechat.Config{BaseURL: cfg.Channels.Wechat.BaseURL}, nil, logf))
			fmt.Println("通道 wechat 启动中（iLink 长轮询，实验性/DM-only）· 接入：tokencode wechat login")
		}
		router.Start(ctx)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	fmt.Printf("tokencode serve · %s · GET /healthz · POST /v1/run（Accept: text/event-stream 走 SSE）· WebUI http://%s/ui · Ctrl-C 退出\n", *addr, *addr)

	select {
	case err := <-errCh:
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	case <-ctx.Done():
		// 优雅退出：停止接新连接，给在跑的 run 一个收尾窗口。
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			fmt.Fprintln(os.Stderr, "warn: shutdown:", err)
		}
		<-errCh // 必然是 http.ErrServerClosed
		fmt.Println("bye")
		return 0
	}
}
