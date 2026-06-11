// Command tokencode 是一个极简的单 agent 编码助手。
// 默认通过 Anthropic 协议接入 DeepSeek；经 config.json 可注册任意 provider。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/headless"
	"github.com/yzfly/tokencode/internal/hooks"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/mcp"
	"github.com/yzfly/tokencode/internal/pulse"
	"github.com/yzfly/tokencode/internal/race"
	"github.com/yzfly/tokencode/internal/session"
	"github.com/yzfly/tokencode/internal/skill"
	"github.com/yzfly/tokencode/internal/subagent"
	"github.com/yzfly/tokencode/internal/tools"
	"github.com/yzfly/tokencode/internal/tui"
	"github.com/yzfly/tokencode/internal/usage"
	"github.com/yzfly/tokencode/internal/workflow"
)

// version 是构建版本（后续经 -ldflags "-X main.version=..." 注入）。
var version = "0.1.0-dev"

func main() {
	// 子命令（auth/models）不进 TUI，先于 flag 解析分发。
	if handled, code := runSubcommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	model := flag.String("model", "", "model：别名、provider/model-id 或裸 model id（默认 $ANTHROPIC_MODEL 或 config 的 default_model）")
	baseURL := flag.String("base-url", "", "覆盖端点 base URL（默认随 provider，或 $ANTHROPIC_BASE_URL）")
	maxTokens := flag.Int("max-tokens", 4096, "max output tokens")
	yolo := flag.Bool("yolo", false, "skip confirmation for write/edit/bash")
	theme := flag.String("theme", envOr("TOKENCODE_THEME", "auto"), "color theme: auto|light|dark")
	heartbeat := flag.Duration("heartbeat", 0, "心跳间隔（如 30m；0=关闭，需显式开启）")
	cont := flag.Bool("continue", false, "继续当前目录最近一次会话")
	resumeID := flag.String("resume", "", "按会话 id 恢复（tokencode -resume <id>）")
	noSession := flag.Bool("no-session", false, "本次会话不落盘")
	workspaceMode := flag.Bool("workspace", false, "工作空间隔离：文件工具只允许访问当前目录之内（含符号链接解析）")
	prompt := flag.String("p", "", "headless：跑一个 turn 后退出；无值且 stdin 是管道时从 stdin 读 prompt")
	outputFmt := flag.String("output", "text", "headless 输出格式：text|json|stream-json（仅 -p 下有效）")
	allowedTools := flag.String("allowed-tools", strings.Join(headless.DefaultAllowed, ","),
		"headless 工具白名单（逗号分隔；-yolo 全放行，其余工具调用直接拒绝）")
	// 裸 `-p`（管道用法）改写成 `-p=` 再解析，等价于 flag.Parse()。
	_ = flag.CommandLine.Parse(normalizeArgs(os.Args[1:]))

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

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	if *workspaceMode {
		if err := tools.SetWorkspace(cwd); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	// headless（-p）：装配同一套 agent 但跳过 TUI/心跳/会话，跑一个 turn 即退出。
	// 用 Visit 判断 flag 是否显式给出——`-p=` + 管道也是合法用法。
	headlessMode := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "p" {
			headlessMode = true
		}
	})
	if headlessMode {
		switch *outputFmt {
		case "text", "json", "stream-json":
		default:
			fmt.Fprintf(os.Stderr, "error: -output 只支持 text|json|stream-json（收到 %q）\n", *outputFmt)
			os.Exit(2)
		}
		p, err := resolveHeadlessPrompt(*prompt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		os.Exit(runHeadless(cfg, modelName, *baseURL, *maxTokens, p, *outputFmt, splitTools(*allowedTools), *yolo))
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

	reg := tools.NewRegistry(tools.Read(), tools.Write(), tools.Edit(), tools.Bash(),
		tools.WebSearch(), tools.WebFetch())
	ag := agent.New(client, reg, tgt.Model, *maxTokens)

	// hooks：全局（config 顶层 "hooks"）+ 项目级（.tokencode/hooks.json）合并。
	// 配置坏了只警告不挡启动；没配置时 hr 为 nil，所有事件点零开销。
	hr, err := hooks.Load(cfg.Hooks, cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warn:", err)
	}
	ag.SetHooks(hr)

	// 子代理与动态工作流：与主 agent 共享注册表、客户端（跟随 /model 热切换）。
	runner := subagent.NewRunner(ag.Client, reg, *maxTokens, subagent.Discover(cwd))
	runner.Resolve = func(name string) (llm.LLM, string, error) {
		t, err := cfg.Resolve(name)
		if err != nil {
			return nil, "", err
		}
		c, _, err := buildClient(t, "")
		if err != nil {
			return nil, "", err
		}
		return c, t.Model, nil
	}
	reg.Add(subagent.NewTool(runner))
	reg.Add(workflow.NewTool(runner))

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

	// SessionStart：装配完成后触发一次。此刻 TUI 还没接管屏幕，
	// hook 的提示先收进开场 notice，进了 TUI 一并展示。
	if hr != nil {
		var hookNotes []string
		hr.Notify = func(s string) { hookNotes = append(hookNotes, s) }
		hr.OnSessionStart()
		if len(hookNotes) > 0 {
			msg := "hook · " + strings.Join(hookNotes, "\nhook · ")
			if notice == "" {
				notice = msg
			} else {
				notice += "\n" + msg
			}
		}
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

	// 竞赛模式（/race）：racer 经子代理运行器生成（绑定各自 worktree 为工具根、
	// 跳过运行器信号量由 race 自管窗口、静默 UI 自动放行），裁判复用主模型。
	racerDef := subagent.Def{Name: "racer", Prompt: race.RacerSystem, Source: "builtin"}
	runRace := func(ctx context.Context, n int, task string, progress func(race.Progress)) (*race.Result, error) {
		return race.Run(ctx, race.Options{
			N:           n,
			Task:        task,
			Concurrency: cfg.Race.Concurrency,
			Check:       cfg.Race.Check,
			RepoRoot:    cwd,
		}, race.Deps{
			Spawn: func(ctx context.Context, i int, prompt, dir string) (string, error) {
				return runner.SpawnDef(ctx, racerDef, prompt, subagent.SpawnOpts{
					Root:  dir,
					Label: fmt.Sprintf("racer#%d", i),
					UI:    &agent.UI{}, // 静默：进度走聚合面板，工具在 worktree 内自动放行
					NoSem: true,
				})
			},
			Complete: func(ctx context.Context, system, user string) (string, error) {
				client, model := ag.Client()
				resp, err := client.Complete(ctx, llm.Request{
					Model:     model,
					System:    system,
					Messages:  []llm.Message{{Role: llm.RoleUser, Text: user}},
					MaxTokens: 512,
				})
				if err != nil {
					return "", err
				}
				// 裁判不走 agent 循环，这里手动记账。
				usage.Log(usage.Record{
					Model:      model,
					Source:     "race:judge",
					In:         resp.Usage.InputTokens,
					Out:        resp.Usage.OutputTokens,
					CacheRead:  resp.Usage.CacheReadTokens,
					CacheWrite: resp.Usage.CacheWriteTokens,
				})
				return resp.Text, nil
			},
			Progress: progress,
		})
	}

	err = tui.Run(ag, tui.Options{
		Model:     tgt.Model,
		BaseURL:   effBaseURL,
		Theme:     *theme,
		Yolo:      *yolo,
		Notice:    notice,
		Events:    events,
		Idle:      idle,
		Pulse:     pl,
		Cfg:       cfg,
		Hooks:     hr,
		Skills:    skills,
		MCP:       mcpMgr,
		Agents:    runner,
		AutoJudge: makeAutoJudge(cfg, ag),
		Workspace: tools.WorkspaceRoot(),
		Version:   version,
		RunRace:   runRace,
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

// makeAutoJudge 构造 auto 模式的权限裁决器：用小模型（config 的 auto_model，
// 未配置时复用主模型）按当前规则状态判定一次工具调用。解析失败返回 err，
// bridge 落回人工确认——裁决器永远不该比人工模式更宽松地放行。
func makeAutoJudge(cfg config.Config, ag *agent.Agent) tui.AutoJudge {
	var judgeClient llm.LLM
	var judgeModel string
	if cfg.AutoModel != "" {
		if t, err := cfg.Resolve(cfg.AutoModel); err == nil {
			if c, _, err := buildClient(t, ""); err == nil {
				judgeClient, judgeModel = c, t.Model
			}
		}
	}
	return func(name string, input json.RawMessage) (bool, string, error) {
		client, model := judgeClient, judgeModel
		if client == nil {
			client, model = ag.Client()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := client.Complete(ctx, llm.Request{
			Model:  model,
			System: autoJudgeSystem(),
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				Text: fmt.Sprintf("tool: %s\ninput: %s", name, input),
			}},
			MaxTokens: 128,
		})
		if err != nil {
			return false, "", err
		}
		// 裁决器不走 agent 循环，这里手动记账。
		usage.Log(usage.Record{
			Model:      model,
			Source:     "auto:judge",
			In:         resp.Usage.InputTokens,
			Out:        resp.Usage.OutputTokens,
			CacheRead:  resp.Usage.CacheReadTokens,
			CacheWrite: resp.Usage.CacheWriteTokens,
		})
		verdict := strings.TrimSpace(resp.Text)
		switch upper := strings.ToUpper(verdict); {
		case strings.HasPrefix(upper, "ALLOW"):
			return true, strings.TrimSpace(verdict[len("ALLOW"):]), nil
		case strings.HasPrefix(upper, "DENY"):
			return false, strings.TrimSpace(verdict[len("DENY"):]), nil
		}
		return false, "", fmt.Errorf("裁决输出无法解析：%.60s", verdict)
	}
}

// autoJudgeSystem 按当前规则状态组装裁决提示：工作目录、工作空间隔离、
// 用户自定义规则（.tokencode/permissions.md，存在时最高优先）。
func autoJudgeSystem() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	ws := "未开启（无路径硬限制，更要从严）"
	if r := tools.WorkspaceRoot(); r != "" {
		ws = "已开启，文件工具已被硬限制在 " + r + " 之内"
	}
	rules := ""
	if b, err := os.ReadFile(".tokencode/permissions.md"); err == nil && strings.TrimSpace(string(b)) != "" {
		rules = "\n\n用户自定义规则（优先于上面的默认规则）：\n" + strings.TrimSpace(string(b))
	}
	return fmt.Sprintf(`你是编码 agent 的权限裁决器，判断一次工具调用能否自动放行。

当前规则状态：
- 工作目录：%s
- 工作空间隔离：%s

默认规则：
- ALLOW：工作目录内的文件写入与修改；构建/测试/格式化/静态检查；git 只读操作（status/diff/log/show）；包管理器与系统信息的查询类命令。
- DENY：删除大范围文件或目录；写工作目录之外；git push、commit --amend、reset --hard 等改写历史或对外发布；安装/卸载软件；修改系统配置；下载并执行内容；任何不可逆或影响外部世界的操作。
- 拿不准一律 DENY。%s

只回一行：ALLOW <一句话理由> 或 DENY <一句话理由>`, cwd, ws, rules)
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
