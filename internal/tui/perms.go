package tui

import (
	"sync"

	"github.com/yzfly/tokencode/internal/permrules"
)

// permMode 是权限模式。循环顺序 plan → review → auto → yolo → plan。
type permMode int

const (
	modePlan   permMode = iota // 只读：read 之外一律拒绝
	modeReview                 // 默认：写类工具逐次确认（y/n/a）
	modeAuto                   // 小模型按规则状态裁决，裁决失败落回人工确认
	modeYolo                   // 全放行
)

// permDecision 是对一次工具调用的裁决。
type permDecision int

const (
	permConfirm permDecision = iota // 需要用户确认
	permAllow                       // 放行
	permReject                      // 拒绝（plan 模式下的写类工具）
)

// perms 是 TUI 与 worker goroutine 共享的权限状态，加锁保护。
type perms struct {
	mu    sync.Mutex
	mode  permMode
	allow map[string]bool // review 模式下本会话已 'a' 放行的工具
}

func newPerms(m permMode) *perms {
	return &perms{mode: m, allow: map[string]bool{}}
}

func (p *perms) current() permMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *perms) setMode(m permMode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

// cycle 切到下一个模式并返回它。
func (p *perms) cycle() permMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = (p.mode + 1) % 4
	return p.mode
}

func (p *perms) rememberAlways(tool string) {
	p.mu.Lock()
	p.allow[tool] = true
	p.mu.Unlock()
}

// decide 裁决一次工具调用。read 永远放行；其余看模式。
// auto 与 review 同样返回 permConfirm——由 bridge 决定确认走人还是走小模型。
func (p *perms) decide(tool string) permDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tool == "read" {
		return permAllow
	}
	switch p.mode {
	case modeYolo:
		return permAllow
	case modePlan:
		return permReject
	default: // modeReview / modeAuto
		if p.allow[tool] {
			return permAllow
		}
		return permConfirm
	}
}

// gateAction 是规则裁决与模式裁决合成后的最终动作。
type gateAction int

const (
	gateAllow        gateAction = iota // 放行
	gateReject                         // 拒绝（deny 规则或 plan 只读铁律）
	gateConfirmHuman                   // 强制人工确认（ask 规则命中，auto/yolo 也要问）
	gateConfirmMode                    // 走模式默认确认路径（auto 可先问小模型）
)

// resolveGate 合成两路裁决（纯函数，便于测试）。优先级：
//
//	deny 规则 > plan 只读铁律 > ask 规则 > allow 规则 > 模式默认。
//
// 即：deny 永远拒；plan 下规则 allow 也突破不了只读；ask 在任何模式下都
// 强制人工确认（CC 语义，yolo/auto 也要问）；allow 跳过确认直接放行。
func resolveGate(rd permrules.Decision, pd permDecision) gateAction {
	if rd == permrules.Deny {
		return gateReject
	}
	switch pd {
	case permReject: // plan 只读铁律：allow/ask 都不突破
		return gateReject
	case permAllow: // read 恒放行 / yolo / 'a' 记住
		if rd == permrules.Ask {
			return gateConfirmHuman
		}
		return gateAllow
	default: // permConfirm（review / auto）
		switch rd {
		case permrules.Allow:
			return gateAllow
		case permrules.Ask:
			return gateConfirmHuman
		}
		return gateConfirmMode
	}
}

func (m permMode) label() string {
	switch m {
	case modePlan:
		return "plan"
	case modeAuto:
		return "auto"
	case modeYolo:
		return "yolo"
	default:
		return "review"
	}
}
