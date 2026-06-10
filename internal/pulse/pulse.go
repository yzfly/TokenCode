// Package pulse 实现心跳与做梦：agent 不只在用户说话时醒。
// 心跳三级短路省 token：L0 本地检查零成本（预期绝大多数拍停在这里）→
// L1 一次调用、空转回哨兵则从历史剔除 → L2 真有事才是完整 turn。
package pulse

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
)

// Check 是一项零 token 的本地自检（L0）。返回非空报告表示有事项，空串表示无事。
type Check func() string

// DefaultHeartbeatInterval 是推荐的心跳间隔。
const DefaultHeartbeatInterval = 30 * time.Minute

// minIdle 是用户活跃窗口：距上次用户输入不足此时长的拍直接跳过。
const minIdle = time.Minute

// Config 是心跳配置。
type Config struct {
	// HeartbeatInterval 是心跳间隔；<=0 表示关闭。
	HeartbeatInterval time.Duration
	// Sentinel 是空转哨兵，默认 agent.Sentinel。
	// v1 历史剔除按 agent.Sentinel 比对，此处自定义值须与之一致。
	Sentinel string
	// Logf 是 debug 日志接收器（每拍的短路去向），nil 则丢弃。
	Logf func(format string, args ...any)
}

// Pulse 是心跳源：按节拍跑 L0 检查，有报告才向事件队列投一拍。
type Pulse struct {
	cfg    Config
	events chan<- agent.Event
	idle   *IdleTracker
	busy   func() bool // 是否有 turn 正在执行（通常是 agent.Busy）
	checks []Check
}

// New 创建心跳源。idle、busy 可为 nil（对应判定跳过）。
func New(cfg Config, events chan<- agent.Event, idle *IdleTracker, busy func() bool, checks ...Check) *Pulse {
	if cfg.Sentinel == "" {
		cfg.Sentinel = agent.Sentinel
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Pulse{cfg: cfg, events: events, idle: idle, busy: busy, checks: checks}
}

// Start 启动心跳循环，阻塞到 ctx 取消。间隔 <=0 时立即返回（关闭）。
func (p *Pulse) Start(ctx context.Context) {
	if p.cfg.HeartbeatInterval <= 0 {
		return
	}
	t := time.NewTicker(p.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.beat(ctx)
		}
	}
}

// beat 执行一拍。心跳是「拍」不是「债」：用户在干活、L0 全空、
// 或队列里已有事件排队时，这一拍直接作废，不补跑。
func (p *Pulse) beat(ctx context.Context) {
	if p.busy != nil && p.busy() {
		p.cfg.Logf("pulse: turn 进行中，跳过")
		return
	}
	if p.idle != nil && p.idle.IdleFor() < minIdle {
		p.cfg.Logf("pulse: 用户刚活跃过（%s 前），跳过", p.idle.IdleFor().Round(time.Second))
		return
	}

	var reports []string
	for _, c := range p.checks {
		if r := c(); r != "" {
			reports = append(reports, r)
		}
	}
	if len(reports) == 0 {
		p.cfg.Logf("pulse: L0 全空，零 token 跳过")
		return
	}

	text := fmt.Sprintf("<heartbeat ts=%s>周期自检，有事项：%s。处理需要处理的；无事可报时仅回复 %s。</heartbeat>",
		time.Now().Format(time.RFC3339), strings.Join(reports, "；"), p.cfg.Sentinel)
	ev := agent.Event{Source: agent.SourceHeartbeat, Text: text, Ephemeral: true}
	select {
	case p.events <- ev:
		p.cfg.Logf("pulse: 升级 L1，事项 %d 条", len(reports))
	case <-ctx.Done():
	default:
		p.cfg.Logf("pulse: 事件队列满，这一拍作废")
	}
}
