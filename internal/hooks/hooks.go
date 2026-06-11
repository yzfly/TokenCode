// Package hooks 实现 Claude Code 风格的命令型 hooks 子集——生态可插拔的最小集：
// PreToolUse / PostToolUse / SessionStart / Stop 四事件，handler 只有 command 一种。
//
// 执行协议（对齐 CC 的 command hook）：
//   - 命令经 `sh -c` 执行，stdin 喂 JSON 事件载荷，env 给便捷字段
//     （TOKENCODE_EVENT / TOKENCODE_TOOL / TOKENCODE_FILE）。
//   - 退出码：PreToolUse 下 exit 2 = 阻断该次工具调用（stderr 作为给模型的拒绝
//     理由）；其余非零退出码只警告不阻断。
//   - stdout 若是 {"systemMessage":"..."} JSON，内容作为 note 提示用户；其余忽略。
//   - 单 hook 超时 30s，超时按非阻断处理并警告。
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 支持的四个事件名（config 键即事件名）。
const (
	EventPreToolUse   = "PreToolUse"
	EventPostToolUse  = "PostToolUse"
	EventSessionStart = "SessionStart"
	EventStop         = "Stop"
)

// ProjectFile 是项目级 hooks 配置的相对路径（相对工作目录/工作空间根）。
const ProjectFile = ".tokencode/hooks.json"

// DefaultTimeout 是单个 hook 命令的执行超时。
const DefaultTimeout = 30 * time.Second

// resultLimit 是 PostToolUse 载荷里 result 字段的截断长度（字符数）。
const resultLimit = 2000

// Hook 是一条 hook 配置。
type Hook struct {
	// Matcher 是对工具名的正则（整名匹配，如 "bash"、"write|edit"；空=全匹配）。
	// 仅 PreToolUse / PostToolUse 有意义，其余事件忽略。
	Matcher string `json:"matcher"`
	// Command 经 `sh -c` 执行。
	Command string `json:"command"`
}

// Config 是配置形态：事件名 → hook 列表。出现在 config.json 顶层 "hooks"
// 与项目级 .tokencode/hooks.json（整个文件就是这个 map）。
type Config map[string][]Hook

// compiledHook 是编译后的一条 hook：matcher 预编译，nil = 全匹配。
type compiledHook struct {
	matcher *regexp.Regexp
	command string
}

// Runner 执行已加载的 hooks。所有方法 nil 接收者安全——无 hook 配置时
// 调用方拿到 nil Runner，每个事件点只是一次 nil 判断，零开销。
// 加载后 hooks 只读，可被并行的工具执行 goroutine 同时调用。
type Runner struct {
	hooks map[string][]compiledHook

	// Timeout 是单 hook 超时；零值用 DefaultTimeout（测试注入短超时用）。
	Timeout time.Duration
	// Notify 是提示去向（systemMessage、阻断与警告），由外壳装配时设置；
	// nil 时静默丢弃。TUI 下接 note，headless 下接 stderr。
	Notify func(text string)
}

// payload 是喂给 hook 命令 stdin 的 JSON 事件载荷。
type payload struct {
	Event   string          `json:"event"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Result  string          `json:"result,omitempty"`   // 仅 PostToolUse（前 2000 字符）
	IsError *bool           `json:"is_error,omitempty"` // 仅 PostToolUse
	Cwd     string          `json:"cwd,omitempty"`
}

// Load 合并全局（config.json 顶层 "hooks"）与项目级（dir/.tokencode/hooks.json）
// 配置并预编译 matcher。项目优先：同一事件下项目 hook 排在全局之前执行。
// 两边都没有任何 hook 时返回 (nil, nil)——nil Runner 即零开销快速路径。
func Load(global Config, dir string) (*Runner, error) {
	var project Config
	p := filepath.Join(dir, ProjectFile)
	raw, err := os.ReadFile(p)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &project); err != nil {
			return nil, fmt.Errorf("hooks: 解析 %s: %w", p, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// 没有项目级配置，正常。
	default:
		return nil, fmt.Errorf("hooks: 读取 %s: %w", p, err)
	}

	compiled := make(map[string][]compiledHook)
	for _, ev := range []string{EventPreToolUse, EventPostToolUse, EventSessionStart, EventStop} {
		merged := append(append([]Hook{}, project[ev]...), global[ev]...)
		for _, h := range merged {
			if strings.TrimSpace(h.Command) == "" {
				return nil, fmt.Errorf("hooks: %s 有一条 hook 缺 command", ev)
			}
			ch := compiledHook{command: h.Command}
			if h.Matcher != "" {
				// 整名匹配："edit" 不该误中 "myedit"；写 "write|edit" 即多选。
				re, err := regexp.Compile("^(?:" + h.Matcher + ")$")
				if err != nil {
					return nil, fmt.Errorf("hooks: %s matcher %q: %w", ev, h.Matcher, err)
				}
				ch.matcher = re
			}
			compiled[ev] = append(compiled[ev], ch)
		}
	}
	if len(compiled) == 0 {
		return nil, nil
	}
	return &Runner{hooks: compiled}, nil
}

// OnPreTool 在工具执行前触发 PreToolUse hooks。任一 hook exit 2 即阻断
// （block=true，reason 取其 stderr），余下 hooks 不再执行。
func (r *Runner) OnPreTool(tool string, input json.RawMessage) (block bool, reason string) {
	if r == nil {
		return false, ""
	}
	return r.fire(EventPreToolUse, payload{Event: EventPreToolUse, Tool: tool, Input: input})
}

// OnPostTool 在工具执行后触发 PostToolUse hooks（含工具出错时）。从不阻断。
func (r *Runner) OnPostTool(tool string, input json.RawMessage, result string, isErr bool) {
	if r == nil {
		return
	}
	r.fire(EventPostToolUse, payload{
		Event:   EventPostToolUse,
		Tool:    tool,
		Input:   input,
		Result:  truncate(result, resultLimit),
		IsError: &isErr,
	})
}

// OnSessionStart 在装配完成、会话开始时触发一次。
func (r *Runner) OnSessionStart() {
	if r == nil {
		return
	}
	r.fire(EventSessionStart, payload{Event: EventSessionStart})
}

// OnStop 在一个 turn 正常结束（模型不再要工具、agent 循环收口）时触发。
func (r *Runner) OnStop() {
	if r == nil {
		return
	}
	r.fire(EventStop, payload{Event: EventStop})
}

// fire 顺序执行一个事件下的全部匹配 hooks，按退出码协议汇总。
func (r *Runner) fire(event string, p payload) (block bool, reason string) {
	hs := r.hooks[event]
	if len(hs) == 0 {
		return false, ""
	}
	if p.Cwd == "" {
		p.Cwd, _ = os.Getwd()
	}
	body, err := json.Marshal(p)
	if err != nil {
		r.notify(fmt.Sprintf("hook 载荷编码失败（%s）：%v", event, err))
		return false, ""
	}
	env := append(os.Environ(), "TOKENCODE_EVENT="+event)
	if p.Tool != "" {
		env = append(env, "TOKENCODE_TOOL="+p.Tool)
	}
	if f := filePath(p.Input); f != "" {
		env = append(env, "TOKENCODE_FILE="+f)
	}

	for _, h := range hs {
		if h.matcher != nil && !h.matcher.MatchString(p.Tool) {
			continue
		}
		code, stdout, stderr, timedOut := r.runOne(h.command, body, env)
		if timedOut {
			r.notify(fmt.Sprintf("hook 超时（%s）已跳过：%s", event, oneLine(h.command)))
			continue
		}
		// stdout 的 systemMessage 协议对任何退出码都生效。
		if msg := systemMessage(stdout); msg != "" {
			r.notify(msg)
		}
		if code == 2 && event == EventPreToolUse {
			reason = strings.TrimSpace(stderr)
			if reason == "" {
				reason = "(hook 未给出理由)"
			}
			r.notify(fmt.Sprintf("PreToolUse 阻断 %s：%s", p.Tool, reason))
			return true, reason
		}
		if code != 0 {
			detail := strings.TrimSpace(stderr)
			if detail != "" {
				detail = "：" + oneLine(detail)
			}
			r.notify(fmt.Sprintf("hook 退出码 %d（%s，不阻断）%s", code, event, detail))
		}
	}
	return false, ""
}

// runOne 跑一条 hook 命令：sh -c、stdin 喂载荷、限时回收。
func (r *Runner) runOne(command string, stdin []byte, env []string) (code int, stdout, stderr string, timedOut bool) {
	to := r.Timeout
	if to <= 0 {
		to = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = env
	// 子进程残留管道不该把我们挂死：杀掉后最多再等 2s 收尾。
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if ctx.Err() != nil {
		return 0, "", "", true
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			// sh 都没起来（极罕见）：按非阻断警告处理。
			code = -1
			errb.WriteString(err.Error())
		}
	}
	return code, out.String(), errb.String(), false
}

func (r *Runner) notify(text string) {
	if r.Notify != nil {
		r.Notify(text)
	}
}

// systemMessage 解析 stdout 的 {"systemMessage":"..."} 协议；不匹配返回空。
func systemMessage(stdout string) string {
	s := strings.TrimSpace(stdout)
	if s == "" || !strings.HasPrefix(s, "{") {
		return ""
	}
	var v struct {
		SystemMessage string `json:"systemMessage"`
	}
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	return v.SystemMessage
}

// filePath 从工具入参里提取 "path" 字段（write/edit/read 等约定参数名），
// 作为 TOKENCODE_FILE 便捷变量；没有就不给。
func filePath(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var v struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &v) != nil {
		return ""
	}
	return v.Path
}

// truncate 按字符（rune）截断到 n，避免拦腰斩断多字节字符。
func truncate(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

// oneLine 把多行文本压成单行摘要（提示用）。
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = truncate(s, 117) + "..."
	}
	return s
}
