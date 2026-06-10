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
	"github.com/yzfly/tokencode/internal/mcp"
	"github.com/yzfly/tokencode/internal/pulse"
	"github.com/yzfly/tokencode/internal/session"
	"github.com/yzfly/tokencode/internal/skill"
	"github.com/yzfly/tokencode/internal/tools"
	"github.com/yzfly/tokencode/internal/tui"
)

// version 是构建版本（后续经 -ldflags "-X main.version=..." 注入）。
var version = "0.1.0-dev"

func main() {
	model := flag.String("model", "", "model：别名、provider/model-id 或裸 model id（默认 $ANTHROPIC_MODEL 或 config 的 default_model）")
	baseURL := flag.String("base-url", "", "覆盖端点 base URL（默认随 provider，或 $ANTHROPIC_BASE_URL）")
	maxTokens := flag.Int("max-tokens", 4096, "max output tokens")
	yolo := flag.Bool("yolo", false, "skip confirmation for write/edit/bash")
	theme := flag.String("theme", envOr("TOKENCODE_THEME", "auto"), "color theme: auto|light|dark")
	heartbeat := flag.Duration("heartbeat", 0, "心跳间隔（如 30m；0=关闭，需显式开启）")
	cont := flag.Bool("continue", false, "继续当前目录最近一次会话")
	resumeID := flag.String("resume", "", "按会话 id 恢复（tokencode -resume <id>）")
	noSession := flag.Bool("no-session", false, "本次会话不落盘")
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

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	reg := tools.NewRegistry(tools.Read(), tools.Write(), tools.Edit(), tools.Bash())
	ag := agent.New(client, reg, tgt.Model, *maxTokens)

	// MCP server 后台连接（绝不阻塞启动）；技能只读 frontmatter，启动开销极小。
	var mcpMgr *mcp.Manager
	if len(cfg.MCP) > 0 {
		mcpMgr = mcp.NewManager(cfg.MCP, reg)
		defer mcpMgr.Close()
	}
	skills := skill.Discover(cwd)

	notice, store, err := setupSession(ag, tgt.Model, *cont, *resumeID, *noSession)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if store != nil {
		defer store.Close()
	}

	events := make(chan agent.Event, 1)
	idle := pulse.NewIdleTracker()
	var pl *pulse.Pulse
	if *heartbeat > 0 {
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
		Notice:  notice,
		Events:  events,
		Idle:    idle,
		Pulse:   pl,
		Cfg:     cfg,
		Skills:  skills,
		MCP:     mcpMgr,
		Version: version,
		SwitchModel: func(name string) (string, string, error) {
			t, err := cfg.Resolve(name)
			if err != nil {
				return "", "", err
			}
			c, burl, err := buildClient(t, "")
			if err != nil {
				return "", "", err
			}
			ag.SetClient(c, t.Model)
			return t.Model, burl, nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// setupSession 装配会话持久化：新建或恢复会话文件，把历史 Seed 进 agent、
// 把追加回调挂上 persist。恢复是用户显式要求的，失败即报错；新建失败只警告
// 降级为不落盘（持久化问题绝不阻塞对话）。
func setupSession(ag *agent.Agent, model string, cont bool, resumeID string, noSession bool) (notice string, store *session.Store, err error) {
	if noSession {
		return "", nil, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	if cont || resumeID != "" {
		id := resumeID
		if id == "" {
			id, err = session.Latest(cwd)
			if err != nil {
				return "", nil, fmt.Errorf("没有可继续的会话（当前目录从未保存过会话）")
			}
		}
		_, msgs, err := session.Load(id)
		if err != nil {
			return "", nil, fmt.Errorf("恢复会话 %s 失败: %w", id, err)
		}
		store, err = session.Open(id)
		if err != nil {
			return "", nil, fmt.Errorf("打开会话 %s 失败: %w", id, err)
		}
		ag.Seed(msgs)
		ag.SetPersist(store.Append)
		return fmt.Sprintf("已恢复会话 %s（%d 条消息）", id, len(msgs)), store, nil
	}

	store, err = session.Create(cwd, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: 会话落盘不可用: %v\n", err)
		return "", nil, nil
	}
	ag.SetPersist(store.Append)
	return "", store, nil
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
	case config.ProtocolOpenAI:
		if burl == "" {
			return nil, "", fmt.Errorf("provider 缺少 base_url（openai 协议必填）")
		}
		return llm.NewOpenAI(tgt.APIKey, burl), burl, nil
	case config.ProtocolGoogle:
		if burl == "" {
			burl = llm.DefaultGoogleBaseURL
		}
		return llm.NewGoogle(tgt.APIKey, burl), burl, nil
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
