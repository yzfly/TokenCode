package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/config"
	"github.com/yzfly/tokencode/internal/headless"
	"github.com/yzfly/tokencode/internal/hooks"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/permrules"
	"github.com/yzfly/tokencode/internal/subagent"
	"github.com/yzfly/tokencode/internal/tools"
	"github.com/yzfly/tokencode/internal/workflow"
)

// normalizeArgs 把裸 `-p`（位于末尾或后面紧跟另一个 flag）改写成 `-p=`：
// flag 包的 string flag 不支持可选值，补上空值才能支持 `... | tokencode -p`
// 的管道用法。代价是 prompt 不能以 "-" 开头（这种写法本身就该用 -p="..."）。
func normalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if (a == "-p" || a == "--p") && (i == len(args)-1 || strings.HasPrefix(args[i+1], "-")) {
			out = append(out, a+"=")
			continue
		}
		out = append(out, a)
	}
	return out
}

// resolveHeadlessPrompt 决定 headless 的 prompt：有 flag 值用值，
// 否则 stdin 是管道/重定向时读完 stdin。
func resolveHeadlessPrompt(flagVal string) (string, error) {
	if p := strings.TrimSpace(flagVal); p != "" {
		return p, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("-p 需要 prompt（直接传参，或经管道喂 stdin）")
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("读 stdin: %w", err)
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return "", fmt.Errorf("stdin 是空的，-p 需要 prompt")
	}
	return p, nil
}

// splitTools 解析 -allowed-tools 的逗号分隔列表（去空白、丢空项）。
func splitTools(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// assembleHeadless 装配一个无界面 agent：模型解析 → 客户端 → 带白名单守卫的
// 工具注册表（含子代理与 workflow，守卫在工具层、子代理经共享注册表自动继承）
// → agent。-p 用一次，serve 每请求用一次（stateless），IM 通道每会话用一次。
// usageSource 标识装配方（"headless" / "serve" / "channel:feishu"），落进用量
// 记账的 Source。root 非空时注册表 SetRoot：文件工具被硬隔离在该工作空间内、
// bash 在其下执行、子代理定义也从那里发现（通道按成员绑定的 workspace 传入）。
func assembleHeadless(cfg config.Config, modelName, baseURLFlag string, maxTokens int, allowed []string, yolo bool, usageSource, root string) (*agent.Agent, string, error) {
	tgt, err := cfg.Resolve(modelName)
	if err != nil {
		return nil, "", err
	}
	client, _, err := buildClient(tgt, baseURLFlag)
	if err != nil {
		return nil, "", err
	}
	cwd := root
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			cwd = "."
		}
	}

	allow := headless.Allow(allowed, yolo)
	// deny 表全局生效：白名单（或 -yolo）放行之前先查 deny（全局 config +
	// 工作空间的 .tokencode/permissions.json）。坏规则警告进 stderr，不阻塞。
	rules, ruleWarns := permrules.Load(cwd, cfg.Permissions)
	for _, w := range ruleWarns {
		fmt.Fprintln(os.Stderr, "warn: 权限规则:", w)
	}
	reg := tools.NewRegistry()
	for _, t := range []tools.Tool{tools.Read(), tools.Write(), tools.Edit(), tools.Bash(),
		tools.WebSearch(), tools.WebFetch()} {
		reg.Add(headless.GateToolRules(t, allow, rules))
	}
	if root != "" {
		reg.SetRoot(root)
	}
	ag := agent.New(client, reg, tgt.Model, maxTokens)
	ag.SetUsageSource(usageSource)

	// hooks 同样生效（headless/serve/channel 都经这里装配）：提示走 stderr，
	// 不碰 stdout 的机器可读输出契约。每次装配即一个新会话，SessionStart 在此触发。
	if hr, err := hooks.Load(cfg.Hooks, cwd); err != nil {
		fmt.Fprintln(os.Stderr, "warn:", err)
	} else if hr != nil {
		hr.Notify = func(s string) { fmt.Fprintln(os.Stderr, "hook:", s) }
		ag.SetHooks(hr)
		hr.OnSessionStart()
	}

	runner := subagent.NewRunner(ag.Client, reg, maxTokens, subagent.Discover(cwd))
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
	reg.Add(headless.GateToolRules(subagent.NewTool(runner), allow, rules))
	reg.Add(headless.GateToolRules(workflow.NewTool(runner), allow, rules))
	return ag, tgt.Model, nil
}

// runHeadless 是 -p 的主流程：装配 → 跑一个 turn → 按 -output 写 stdout。
// 返回进程退出码：成功 0，模型/网络/装配错误 1。
func runHeadless(cfg config.Config, modelName, baseURL string, maxTokens int, prompt, output string, allowed []string, yolo bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ag, model, err := assembleHeadless(cfg, modelName, baseURL, maxTokens, allowed, yolo, "headless", "")
	if err != nil {
		emitHeadlessFailure(output, modelName, err)
		return 1
	}
	res := headless.Execute(ctx, ag, model, prompt, output, os.Stdout)
	if res.IsError {
		if output == "text" || output == "" {
			fmt.Fprintln(os.Stderr, "error:", res.Result)
		}
		return 1
	}
	return 0
}

// emitHeadlessFailure 按输出格式报告一次没跑起来的失败（装配/解析错误）：
// json/stream-json 维持机器可读契约（is_error=true），text 走 stderr。
func emitHeadlessFailure(output, model string, err error) {
	switch output {
	case "json":
		_ = json.NewEncoder(os.Stdout).Encode(headless.Result{Result: err.Error(), Model: model, IsError: true})
	case "stream-json":
		n := 0
		_ = json.NewEncoder(os.Stdout).Encode(headless.Event{Type: "result", Result: err.Error(), ToolCalls: &n, IsError: true})
	default:
		fmt.Fprintln(os.Stderr, "error:", err)
	}
}
