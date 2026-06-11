package wechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store 管微信账号的全部落盘状态，目录 ~/.config/tokencode/wechat/：
//
//   - <account_id>.json         凭证（bot_token/baseurl/账号信息），0600；
//   - <account_id>.cursor.json  getupdates 游标（断点续传，丢了会重放或漏消息）；
//   - <account_id>.tokens.json  context_token 按 peer 持久化（丢了重启后回不了话）。
//
// 全部临时文件 + rename 原子落盘（与 internal/auth、channel.Store 同款手法）。
type Store struct {
	dir string
}

// Credential 是一个已登录账号的凭证（扫码 confirmed 时服务端下发）。
type Credential struct {
	AccountID string `json:"account_id"` // ilink_bot_id（xxx@im.bot）
	BotToken  string `json:"bot_token"`
	BaseURL   string `json:"baseurl"` // 服务端下发的专属基座，后续请求要跟随
	UserID    string `json:"ilink_user_id"`
	SavedAt   string `json:"saved_at"`
}

// NewStore 创建 store。dir 为空用默认目录（XDG_CONFIG_HOME 或 ~/.config 下的
// tokencode/wechat）；测试传临时目录。
func NewStore(dir string) *Store {
	if dir == "" {
		dir = defaultDir()
	}
	return &Store{dir: dir}
}

// Dir 返回存储目录。
func (s *Store) Dir() string { return s.dir }

func defaultDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tokencode", "wechat")
}

// SaveCredential 落盘凭证（0600）。
func (s *Store) SaveCredential(c Credential) error {
	if c.AccountID == "" {
		return errors.New("wechat: 凭证缺 account_id")
	}
	if c.SavedAt == "" {
		c.SavedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return s.writeJSON(s.credPath(c.AccountID), c)
}

// List 列出全部已登录账号（按 account_id 排序）。目录不存在返回空。
func (s *Store) List() ([]Credential, error) {
	ents, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Credential
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") ||
			strings.HasSuffix(name, ".cursor.json") || strings.HasSuffix(name, ".tokens.json") {
			continue
		}
		var c Credential
		if err := s.readJSON(filepath.Join(s.dir, name), &c); err != nil {
			continue // 坏文件跳过，不拖垮整体
		}
		if c.AccountID != "" && c.BotToken != "" {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

// Remove 删除一个账号的凭证与全部状态文件。账号不存在报错。
func (s *Store) Remove(accountID string) error {
	if err := os.Remove(s.credPath(accountID)); err != nil {
		return err
	}
	_ = os.Remove(s.cursorPath(accountID))
	_ = os.Remove(s.tokensPath(accountID))
	return nil
}

// LoadCursor 读 getupdates 游标；没有返回空串（从头拉）。
func (s *Store) LoadCursor(accountID string) string {
	var v struct {
		Buf string `json:"get_updates_buf"`
	}
	if err := s.readJSON(s.cursorPath(accountID), &v); err != nil {
		return ""
	}
	return v.Buf
}

// SaveCursor 落盘游标。
func (s *Store) SaveCursor(accountID, buf string) error {
	return s.writeJSON(s.cursorPath(accountID), map[string]string{"get_updates_buf": buf})
}

// LoadTokens 读该账号全部 peer 的 context_token（peer → token）。
func (s *Store) LoadTokens(accountID string) map[string]string {
	out := map[string]string{}
	_ = s.readJSON(s.tokensPath(accountID), &out)
	return out
}

// SaveToken 持久化一个 peer 的 context_token（读-改-写整个文件；写入频率
// 跟着入站消息走，量级很小）。
func (s *Store) SaveToken(accountID, peer, token string) error {
	m := s.LoadTokens(accountID)
	m[peer] = token
	return s.writeJSON(s.tokensPath(accountID), m)
}

func (s *Store) credPath(accountID string) string {
	return filepath.Join(s.dir, sanitize(accountID)+".json")
}
func (s *Store) cursorPath(accountID string) string {
	return filepath.Join(s.dir, sanitize(accountID)+".cursor.json")
}
func (s *Store) tokensPath(accountID string) string {
	return filepath.Join(s.dir, sanitize(accountID)+".tokens.json")
}

// sanitize 把 account_id 变成安全文件名（防路径穿越；@ 和 . 保留可读性）。
func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '@', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

func (s *Store) readJSON(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// writeJSON 原子落盘：临时文件 + rename，权限 0600（凭证与会话票据都敏感）。
func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
