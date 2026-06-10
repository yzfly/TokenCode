package tui

import "sync"

// permMode 是权限模式。循环顺序 plan → review → yolo → plan。
type permMode int

const (
	modePlan   permMode = iota // 只读：read 之外一律拒绝
	modeReview                 // 默认：写类工具逐次确认（y/n/a）
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
	p.mode = (p.mode + 1) % 3
	return p.mode
}

func (p *perms) rememberAlways(tool string) {
	p.mu.Lock()
	p.allow[tool] = true
	p.mu.Unlock()
}

// decide 裁决一次工具调用。read 永远放行；其余看模式。
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
	default: // modeReview
		if p.allow[tool] {
			return permAllow
		}
		return permConfirm
	}
}

func (m permMode) label() string {
	switch m {
	case modePlan:
		return "plan"
	case modeYolo:
		return "yolo"
	default:
		return "review"
	}
}
