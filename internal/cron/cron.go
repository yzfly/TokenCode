// Package cron 实现进程内定时任务：到点把 prompt 作为一拍投给主 agent 的
// 事件队列（与心跳/梦共用 Event 原语）。任务只活在本进程内，不落盘——
// 这是「会话内的闹钟」，不是系统级 crontab。
package cron

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// 失控防线：任务数上限；MinEvery 是最小间隔（烧得其所，绝不空烧——
// 太密的定时拍就是空烧 token）。变量而非常量：测试可缩短。
const maxEntries = 20

var MinEvery = time.Minute

// Entry 是一个定时任务的对外快照。
type Entry struct {
	Name   string
	Every  time.Duration
	Prompt string
	NextAt time.Time
	Runs   int
}

type entry struct {
	Entry
	timer *time.Timer
}

// Manager 管理定时任务的生命周期。emit 在任务到点时被调用（独立 goroutine，
// 可阻塞——投递给繁忙的 agent 时等待是预期行为）。
type Manager struct {
	mu      sync.Mutex
	emit    func(name, prompt string)
	entries map[string]*entry
	closed  bool
}

// NewManager 创建定时任务管理器。
func NewManager(emit func(name, prompt string)) *Manager {
	return &Manager{emit: emit, entries: map[string]*entry{}}
}

// Create 注册一个周期任务：每 every 触发一次，把 prompt 投给主 agent。
func (m *Manager) Create(name string, every time.Duration, prompt string) error {
	if name == "" {
		return fmt.Errorf("任务名不能为空")
	}
	if prompt == "" {
		return fmt.Errorf("prompt 不能为空")
	}
	if every < MinEvery {
		return fmt.Errorf("间隔至少 %s（收到 %s）——太密的定时拍是空烧 token", MinEvery, every)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("管理器已关闭")
	}
	if _, ok := m.entries[name]; ok {
		return fmt.Errorf("任务 %q 已存在（先 cron_delete 再重建）", name)
	}
	if len(m.entries) >= maxEntries {
		return fmt.Errorf("任务数已达上限 %d", maxEntries)
	}
	e := &entry{Entry: Entry{Name: name, Every: every, Prompt: prompt, NextAt: time.Now().Add(every)}}
	e.timer = time.AfterFunc(every, func() { m.fire(name) })
	m.entries[name] = e
	return nil
}

// fire 是到点回调：更新记账、重排下一次，然后在锁外投递。
func (m *Manager) fire(name string) {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok || m.closed {
		m.mu.Unlock()
		return
	}
	e.Runs++
	e.NextAt = time.Now().Add(e.Every)
	e.timer.Reset(e.Every)
	emit, prompt := m.emit, e.Prompt
	m.mu.Unlock()
	// 投递可能阻塞（agent 在忙）——绝不持锁调用。
	emit(name, prompt)
}

// Delete 删除任务并停掉它的定时器。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return fmt.Errorf("没有名为 %q 的任务", name)
	}
	e.timer.Stop()
	delete(m.entries, name)
	return nil
}

// List 返回全部任务快照（按名排序）。
func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.Entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Close 停掉全部定时器；之后 Create/fire 都是 no-op。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, e := range m.entries {
		e.timer.Stop()
	}
	m.entries = map[string]*entry{}
}
