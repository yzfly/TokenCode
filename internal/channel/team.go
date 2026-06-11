package channel

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Binding 是「IM 身份 → 工作空间」的一条绑定：路由的单一事实源。
type Binding struct {
	Channel      string    `json:"channel"`                 // 通道名（"feishu"…）
	UserID       string    `json:"user_id"`                 // 通道侧用户标识
	Name         string    `json:"name,omitempty"`          // 备注名
	Workspace    string    `json:"workspace"`               // 工作空间根（绝对路径）
	AllowedTools []string  `json:"allowed_tools,omitempty"` // 工具白名单；空=默认只读集
	Yolo         bool      `json:"yolo,omitempty"`          // 全放行（信任成员才开）
	Model        string    `json:"model,omitempty"`         // 专属模型；空=服务默认
	PairedAt     time.Time `json:"paired_at,omitempty"`     // 配对时间
}

// Pending 是一个待认领的配对码：`tokencode team pair` 生成，陌生用户在 IM
// 里发码即认领成绑定。单次有效，过期自动清理。
type Pending struct {
	Code         string    `json:"code"`
	Name         string    `json:"name,omitempty"`
	Workspace    string    `json:"workspace"`
	AllowedTools []string  `json:"allowed_tools,omitempty"`
	Yolo         bool      `json:"yolo,omitempty"`
	Model        string    `json:"model,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Team 是 team.json 的全貌。
type Team struct {
	Bindings []Binding `json:"bindings"`
	Pending  []Pending `json:"pending,omitempty"`
}

const (
	// MaxPending 是同时存在的待配对码上限。
	MaxPending = 3
	// PairTTL 是配对码有效期。
	PairTTL = time.Hour
	// codeLen 是配对码长度。
	codeLen = 8
	// codeCharset 去掉了易混字符（0O1I），全大写。32 字符 × 8 位 = 40 bit 熵。
	codeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// Store 管 team.json 的读写：0600、临时文件 + rename 原子落盘（与 internal/auth
// 同款做法）。进程内方法串行（mu）；CLI 与 serve 是两个进程，写写竞争窗口极小
// 且 v0 可接受（最坏丢一条 pending，重新 pair 即可）。
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore 创建一个 store。path 空时用默认路径（config 同目录的 team.json）。
func NewStore(path string) *Store {
	if path == "" {
		path = DefaultTeamPath()
	}
	return &Store{path: path}
}

// Path 返回 store 实际使用的文件路径。
func (s *Store) Path() string { return s.path }

// DefaultTeamPath 返回 team.json 默认路径（与 config.json 同目录）。
func DefaultTeamPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tokencode", "team.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "tokencode", "team.json")
}

// Load 读取全量数据。文件不存在返回零值（不是错误）。
func (s *Store) Load() (Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() (Team, error) {
	if s.path == "" {
		return Team{}, errors.New("team: 无法定位配置目录")
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Team{}, nil
	}
	if err != nil {
		return Team{}, err
	}
	var t Team
	if err := json.Unmarshal(raw, &t); err != nil {
		return Team{}, fmt.Errorf("team: parse %s: %w", s.path, err)
	}
	return t, nil
}

// save 原子落盘：临时文件 + rename，权限 0600（绑定里有工作空间路径，按敏感对待）。
func (s *Store) save(t Team) error {
	if s.path == "" {
		return errors.New("team: 无法定位配置目录")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Find 查一条绑定（channel+user_id 精确匹配）。
func (s *Store) Find(channel, userID string) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load()
	if err != nil {
		return Binding{}, false, err
	}
	for _, b := range t.Bindings {
		if b.Channel == channel && b.UserID == userID {
			return b, true, nil
		}
	}
	return Binding{}, false, nil
}

// AddPending 登记一个待配对码：先清过期，再检查上限与码冲突。
func (s *Store) AddPending(p Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load()
	if err != nil {
		return err
	}
	t.Pending = prunePending(t.Pending, time.Now())
	if len(t.Pending) >= MaxPending {
		return fmt.Errorf("team: 待配对码已达上限（%d 个），等成员认领或过期后再生成", MaxPending)
	}
	for _, q := range t.Pending {
		if q.Code == p.Code {
			return fmt.Errorf("team: 配对码冲突，请重试")
		}
	}
	t.Pending = append(t.Pending, p)
	return s.save(t)
}

// Pair 用配对码认领绑定：码命中且未过期 → 删除 pending、写入绑定（单次有效）。
// 返回 ok=false 表示码不存在/已过期（不是错误，路由据此回提示）。
// 同一身份重复配对视为换绑：旧绑定被新的覆盖。
func (s *Store) Pair(channel, userID, userName, code string) (Binding, bool, error) {
	code = NormalizeCode(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load()
	if err != nil {
		return Binding{}, false, err
	}
	now := time.Now()
	t.Pending = prunePending(t.Pending, now)
	idx := -1
	for i, p := range t.Pending {
		if p.Code == code {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 即便没命中也把过期清理落盘（容量尽快释放）；失败不致命，忽略。
		_ = s.save(t)
		return Binding{}, false, nil
	}
	p := t.Pending[idx]
	t.Pending = append(t.Pending[:idx], t.Pending[idx+1:]...)
	name := p.Name
	if name == "" {
		name = userName
	}
	b := Binding{
		Channel:      channel,
		UserID:       userID,
		Name:         name,
		Workspace:    p.Workspace,
		AllowedTools: p.AllowedTools,
		Yolo:         p.Yolo,
		Model:        p.Model,
		PairedAt:     now,
	}
	replaced := false
	for i := range t.Bindings {
		if t.Bindings[i].Channel == channel && t.Bindings[i].UserID == userID {
			t.Bindings[i] = b
			replaced = true
			break
		}
	}
	if !replaced {
		t.Bindings = append(t.Bindings, b)
	}
	if err := s.save(t); err != nil {
		return Binding{}, false, err
	}
	return b, true, nil
}

// Remove 删除一条绑定。返回是否真的删了。
func (s *Store) Remove(channel, userID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load()
	if err != nil {
		return false, err
	}
	for i, b := range t.Bindings {
		if b.Channel == channel && b.UserID == userID {
			t.Bindings = append(t.Bindings[:i], t.Bindings[i+1:]...)
			return true, s.save(t)
		}
	}
	return false, nil
}

// prunePending 丢弃已过期的配对码。
func prunePending(ps []Pending, now time.Time) []Pending {
	out := ps[:0]
	for _, p := range ps {
		if now.Before(p.ExpiresAt) {
			out = append(out, p)
		}
	}
	return out
}

// GenCode 生成 8 位配对码（加密随机，去易混字符）。
func GenCode() (string, error) {
	buf := make([]byte, codeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("team: 生成配对码: %w", err)
	}
	for i, b := range buf {
		buf[i] = codeCharset[int(b)%len(codeCharset)]
	}
	return string(buf), nil
}

// NormalizeCode 把用户输入归一成配对码形态（去空白、转大写）。
func NormalizeCode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// LooksLikeCode 报告一段文本是否像配对码（归一后恰为 8 位且全在字符集内）。
// 用于未绑定用户的消息分流：像码才查 pending，不像直接回引导语。
func LooksLikeCode(s string) bool {
	c := NormalizeCode(s)
	if len(c) != codeLen {
		return false
	}
	for i := 0; i < len(c); i++ {
		if !strings.ContainsRune(codeCharset, rune(c[i])) {
			return false
		}
	}
	return true
}
