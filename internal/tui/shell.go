package tui

// ! shell 直通：输入框里 ! 开头的行直接跑 shell。命令是用户亲手输入的，
// 不走权限确认；输出回显转写区，并包成 <shell-*> 块缓存，附加到下一条
// 用户消息前面——模型看到的效果等同用户手工粘贴了命令和输出
// （与 Claude Code 的 ! 模式语义一致：进上下文，但不触发模型调用）。
// !! 开头是转义：剥掉一个 ! 后作为普通消息发出（与 // 同一惯例）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	shellCtxCap  = 8000            // 进模型上下文的单次输出上限（字节）
	shellShowMax = 30              // 转写区显示的最大输出行数
	shellTimeout = 5 * time.Minute // 单条命令的执行上限
)

// shellDoneMsg 是一条 ! 命令跑完后投回事件循环的结果。
type shellDoneMsg struct {
	cmd  string
	out  string
	exit int
}

// runShell 异步执行一条 ! 命令（不阻塞输入，用户可以继续打字）。
func runShell(cmd string) tea.Cmd {
	return func() tea.Msg {
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "/bin/sh"
		}
		ctx, cancel := context.WithTimeout(context.Background(), shellTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, sh, "-c", cmd).CombinedOutput()
		exit := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else {
				out = append(out, []byte(err.Error())...)
				exit = -1
			}
		}
		if ctx.Err() == context.DeadlineExceeded {
			out = append(out, fmt.Sprintf("\n[timed out after %s]", shellTimeout)...)
		}
		return shellDoneMsg{cmd: cmd, out: string(out), exit: exit}
	}
}

// shellCtxBlock 把一次执行包装成进模型上下文的块。
func shellCtxBlock(cmd, out string, exit int) string {
	if len(out) > shellCtxCap {
		out = out[:shellCtxCap] + "\n[输出过长，已截断]"
	}
	return fmt.Sprintf("<shell-input>%s</shell-input>\n<shell-output exit=\"%d\">\n%s</shell-output>",
		cmd, exit, strings.TrimRight(out, "\n")+"\n")
}

// takeShellCtx 把缓存的 ! 命令输出附加到 outgoing 消息前面，并清空缓冲。
// 用户在两条消息之间跑的所有 ! 命令，随下一条消息一并进入模型上下文。
func (m *model) takeShellCtx(text string) string {
	if len(m.shellCtx) == 0 {
		return text
	}
	blocks := strings.Join(m.shellCtx, "\n")
	m.shellCtx = nil
	return "以下是用户在终端里手动执行的命令与输出：\n\n" + blocks + "\n\n" + text
}
