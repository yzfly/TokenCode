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
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/serve"
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
		def = os.Getenv("ANTHROPIC_MODEL")
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
			return assembleHeadless(cfg, name, "", *maxTokens, allowed, false)
		},
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	fmt.Printf("tokencode serve · %s · GET /healthz · POST /v1/run（Accept: text/event-stream 走 SSE）· Ctrl-C 退出\n", *addr)

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
