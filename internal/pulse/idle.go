package pulse

import (
	"sync/atomic"
	"time"
)

// IdleTracker 记录最近一次用户活动时刻，跨 goroutine 安全。
// TUI 在用户每次提交输入时 Touch，心跳/做梦据 IdleFor 判定空闲。
type IdleTracker struct {
	last atomic.Int64 // UnixNano
}

// NewIdleTracker 创建追踪器，并把「现在」记作最近活动（启动即视为刚活跃）。
func NewIdleTracker() *IdleTracker {
	t := &IdleTracker{}
	t.Touch()
	return t
}

// Touch 记录一次用户活动。
func (t *IdleTracker) Touch() {
	t.last.Store(time.Now().UnixNano())
}

// IdleFor 返回距最近一次活动的时长。
func (t *IdleTracker) IdleFor() time.Duration {
	return time.Duration(time.Now().UnixNano() - t.last.Load())
}
