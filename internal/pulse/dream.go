package pulse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/usage"
)

// DreamConfig 是做梦配置。零值字段在 NewDreamer 里落默认。
type DreamConfig struct {
	AfterIdle   time.Duration // 空闲多久才允许做梦，默认 10m
	MinNewMsgs  int           // 自上个梦后至少新增多少条历史（没有新材料的梦是复读），默认 8
	MinInterval time.Duration // 两梦最小间隔，默认 1h
	MaxPerDay   int           // 每日上限，默认 6
	Model       string        // 做梦模型；v1 复用主模型（便宜档选择待多 provider 接入 config）
	MaxTokens   int           // 默认 2048
	MemoryPath  string        // 默认 agent.MemoryPath
}

// historyCharLimit 是喂给梦的对话摘录上限，超出时保留尾部（近期优先）。
const historyCharLimit = 24000

// Dreamer 在空闲且有新材料时把对话压缩进 memory.md。
// 它是记忆文件的唯一写者；读历史只经快照，不碰 agent 内部状态。
type Dreamer struct {
	cfg DreamConfig
	llm llm.LLM

	sem chan struct{} // cap=1：同时最多一个梦

	mu      sync.Mutex
	last    time.Time // 上次开梦时刻（按开梦计，失败也占间隔，避免坏端点被锤）
	day     time.Time // 计数所属的日子
	today   int       // 当日已开的梦数
	seenLen int       // 上个梦消化到的历史长度
}

// NewDreamer 创建 dreamer，client 通常是主 LLM client。
func NewDreamer(cfg DreamConfig, client llm.LLM) *Dreamer {
	if cfg.AfterIdle <= 0 {
		cfg.AfterIdle = 10 * time.Minute
	}
	if cfg.MinNewMsgs <= 0 {
		cfg.MinNewMsgs = 8
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = time.Hour
	}
	if cfg.MaxPerDay <= 0 {
		cfg.MaxPerDay = 6
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 2048
	}
	if cfg.MemoryPath == "" {
		cfg.MemoryPath = agent.MemoryPath
	}
	return &Dreamer{cfg: cfg, llm: client, sem: make(chan struct{}, 1)}
}

// Due 零成本判定现在是否该做梦：空闲够久 ∧ 有新材料 ∧ 间隔/每日上限未超。
func (d *Dreamer) Due(idle time.Duration, newMsgs int, now time.Time) bool {
	if idle < d.cfg.AfterIdle || newMsgs < d.cfg.MinNewMsgs {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !sameDay(d.day, now) {
		d.day = now
		d.today = 0
	}
	if d.today >= d.cfg.MaxPerDay {
		return false
	}
	if !d.last.IsZero() && now.Sub(d.last) < d.cfg.MinInterval {
		return false
	}
	return true
}

// Check 把做梦挂进心跳 L0：到点就 fork 一个梦，本身永远返回空串。
// v1 梦醒不投事件、只重写 memory.md——下个 turn 重建 system prompt 时自然生效，
// 省一次 LLM 往返，也避免梦醒通知反过来占 turn 队列。
func (d *Dreamer) Check(snapshot func() []llm.Message, idle *IdleTracker) Check {
	return func() string {
		history := snapshot()
		if !d.Due(idle.IdleFor(), len(history)-d.seen(), time.Now()) {
			return ""
		}
		go func() {
			// 梦的 ctx 独立于任何 turn：不随用户输入取消，只设兜底超时。
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = d.Dream(ctx, history)
		}()
		return ""
	}
}

// Dream 做一个梦（ACE 式反思-策展）：现有 playbook 条目 + 历史快照 →
// 一次无工具调用产出增量操作（ADD/UPDATE/DELETE）→ Go 确定性合并后原子写回。
// LLM 不重写整个文件：一次坏梦最多污染几条，毁不掉全部记忆。
// 已有梦在做时直接返回 nil（cap=1，不排队）。
func (d *Dreamer) Dream(ctx context.Context, history []llm.Message) error {
	select {
	case d.sem <- struct{}{}:
	default:
		return nil
	}
	defer func() { <-d.sem }()

	now := time.Now()
	d.mu.Lock()
	if !sameDay(d.day, now) {
		d.day = now
		d.today = 0
	}
	d.last = now
	d.today++
	d.mu.Unlock()

	old, _ := os.ReadFile(d.cfg.MemoryPath)
	entries, legacy := parsePlaybook(string(old))
	resp, err := d.llm.Complete(ctx, llm.Request{
		Model:     d.cfg.Model,
		System:    dreamSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: dreamPrompt(entries, legacy, history)}},
		MaxTokens: d.cfg.MaxTokens,
	})
	if err != nil {
		return err
	}
	// 梦不走 agent 循环，这里手动记账。
	usage.Log(usage.Record{
		Model:      d.cfg.Model,
		Source:     "dream",
		In:         resp.Usage.InputTokens,
		Out:        resp.Usage.OutputTokens,
		CacheRead:  resp.Usage.CacheReadTokens,
		CacheWrite: resp.Usage.CacheWriteTokens,
	})
	// 材料已消化，无论有没有洞察都推进水位线——同一段历史不喂第二次。
	defer func() {
		d.mu.Lock()
		d.seenLen = len(history)
		d.mu.Unlock()
	}()

	ops := parseOps(resp.Text)
	// 无操作且没有待收编的旧格式内容：这一觉无话可说，不碰文件。
	if len(ops) == 0 && legacy == "" {
		return nil
	}
	// legacy 存在时即使零操作也重写——旧自由格式就此谢幕，防止反复喂给梦。
	return atomicWrite(d.cfg.MemoryPath, renderPlaybook(applyOps(entries, ops)))
}

func (d *Dreamer) seen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seenLen
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

var dreamSystem = fmt.Sprintf(
	`你是编码 agent 的记忆整理器（Reflector）。长期记忆是条目化 playbook，
每条是一个可复用的规律：事实、用户偏好、约定、踩过的坑、验证有效的做法。
根据现有条目与最近对话，只输出增量操作（每行一个）：

ADD <id> | <一句话内容>      新增条目（id 用小写短横线短语，如 build-cmd）
UPDATE <id> | <一句话内容>   改写已有条目
DELETE <id>                  删除过时或被证伪的条目
NOOP                         没有值得更新的内容

规则：
- 洞察必须锚定可验证证据：测试结果、命令退出码、工具报错、用户原话。
  没有外部证据的印象与推测不进 playbook——宁缺毋滥。
- 每条一个规律，≤120 字；合并重复；一次性细节不收。
- 每次最多 %d 个操作。只输出操作行，不要解释、不要代码围栏。`, maxOpsPerDream)

func dreamPrompt(entries []playEntry, legacy string, history []llm.Message) string {
	var b strings.Builder
	b.WriteString("## 现有 playbook 条目\n")
	if len(entries) == 0 {
		b.WriteString("（空）\n")
	} else {
		for _, e := range entries {
			fmt.Fprintf(&b, "- [%s] %s\n", e.ID, e.Text)
		}
	}
	if legacy != "" {
		b.WriteString("\n## 旧自由格式记忆（本次之后将被丢弃，值得保留的用 ADD 收编成条目）\n")
		b.WriteString(legacy)
		b.WriteString("\n")
	}
	b.WriteString("\n## 最近对话\n")
	b.WriteString(digest(history))
	b.WriteString("\n请输出增量操作。")
	return b.String()
}

// digest 把历史压成纯文本摘录：工具调用只留名字，结果截短，整体超限保尾部。
func digest(history []llm.Message) string {
	var b strings.Builder
	for _, m := range history {
		if t := strings.TrimSpace(m.Text); t != "" {
			b.WriteString(m.Role + ": " + clipHead(t, 2000) + "\n")
		}
		for _, tu := range m.ToolUses {
			fmt.Fprintf(&b, "%s: [调用工具 %s]\n", m.Role, tu.Name)
		}
		for _, tr := range m.ToolResults {
			if t := strings.TrimSpace(tr.Content); t != "" {
				b.WriteString("tool: " + clipHead(t, 300) + "\n")
			}
		}
	}
	return clipTail(b.String(), historyCharLimit)
}

func clipHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func clipTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// atomicWrite 原子重写：写同目录临时文件再 rename，读者永远看不到半截文件。
func atomicWrite(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
