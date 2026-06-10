// Command tokencode 是一个极简的单 agent 编码助手。
// 默认通过 Anthropic 协议接入 DeepSeek；经 config.json 可注册任意 provider。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/pulse"
	"github.com/yzfly/tokencode/internal/tools"
	"github.com/yzfly/tokencode/internal/tui"
)

func main() {
	model := flag.String("model", "", "model：别名、provider/model-id 或裸 model id（默认 $ANTHROPIC_MODEL 或 config 的 default_model）")
	baseURL := flag.String("base-url", "", "覆盖端点 base URL（默认随 provider，或 $ANTHROPIC_BASE_URL）")
	maxTokens := flag.Int("max-tokens", 4096, "max output tokens")
	yolo := flag.Bool("yolo", false, "skip confirmation for write/edit/bash")
	theme := flag.String("theme", envOr("TOKENCODE_THEME", "auto"), "color theme: auto|light|dark")
	heartbeat := flag.Duration("heartbeat", 0, "心跳间隔（如 30m；0=关闭，需显式开启）")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// 模型名优先级：flag > env > config > 内置默认。
	modelName := *model
	if modelName == "" {
		modelName = os.Getenv("ANTHROPIC_MODEL")
	}
	if modelName == "" {
		modelName = cfg.DefaultModel
	}
	if modelName == "" {
		modelName = llm.DefaultModel
	}

	tgt, err := cfg.Resolve(modelName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	client, effBaseURL, err := buildClient(tgt, *baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	reg := tools.NewRegistry(tools.Read(), tools.Write(), tools.Edit(), tools.Bash())
	ag := agent.New(client, reg, tgt.Model, *maxTokens)

	events := make(chan agent.Event, 1)
	idle := pulse.NewIdleTracker()
	var pl *pulse.Pulse
	if *heartbeat > 0 {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		// 做梦 v1 复用主 client 与主模型（便宜档的 DreamModel 待多 provider 配置接入）。
		dreamer := pulse.NewDreamer(pulse.DreamConfig{Model: tgt.Model}, client)
		pl = pulse.New(
			pulse.Config{HeartbeatInterval: *heartbeat, Logf: pulseLogf()},
			events, idle, ag.Busy,
			pulse.WorkspaceCheck(cwd),
			pulse.TodoCheck(".tokencode/todo.md"),
			dreamer.Check(ag.Snapshot, idle),
		)
	}

	err = tui.Run(ag, tui.Options{
		Model:   tgt.Model,
		BaseURL: effBaseURL,
		Theme:   *theme,
		Yolo:    *yolo,
		Events:  events,
		Idle:    idle,
		Pulse:   pl,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// pulseLogf 返回心跳 debug 日志的去向：TOKENCODE_PULSE_LOG 指定文件时写入，
// 否则丢弃（TUI 占着终端，不能直接打 stdout）。
func pulseLogf() func(string, ...any) {
	path := os.Getenv("TOKENCODE_PULSE_LOG")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return log.New(f, "", log.LstdFlags).Printf
}

// buildClient 按解析落点构造客户端，返回实际生效的 base URL（供 TUI 展示）。
// 落点为 Default 时走 ANTHROPIC_* env 兜底，与无 config 时代行为一致。
func buildClient(tgt config.Target, baseURLFlag string) (llm.LLM, string, error) {
	if tgt.Default {
		burl := baseURLFlag
		if burl == "" {
			burl = envOr("ANTHROPIC_BASE_URL", llm.DefaultBaseURL)
		}
		key, bearer, err := resolveAuth()
		if err != nil {
			return nil, "", err
		}
		return llm.NewAnthropic(key, burl, bearer), burl, nil
	}

	burl := tgt.BaseURL
	if baseURLFlag != "" {
		burl = baseURLFlag
	}
	switch tgt.Protocol {
	case config.ProtocolAnthropic:
		if burl == "" {
			burl = llm.DefaultBaseURL
		}
		return llm.NewAnthropic(tgt.APIKey, burl, tgt.Bearer), burl, nil
	case config.ProtocolOpenAIChat:
		if burl == "" {
			return nil, "", fmt.Errorf("provider 缺少 base_url（openai-chat 协议必填）")
		}
		return llm.NewOpenAI(tgt.APIKey, burl), burl, nil
	default:
		return nil, "", fmt.Errorf("未知协议 %q", tgt.Protocol)
	}
}

// resolveAuth 优先用 ANTHROPIC_AUTH_TOKEN（Bearer），否则 ANTHROPIC_API_KEY（x-api-key）。
func resolveAuth() (key string, bearer bool, err error) {
	if t := os.Getenv("ANTHROPIC_AUTH_TOKEN"); t != "" {
		return t, true, nil
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k, false, nil
	}
	return "", false, fmt.Errorf("set ANTHROPIC_AUTH_TOKEN (DeepSeek key) or ANTHROPIC_API_KEY")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
