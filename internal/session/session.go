// Package session 是会话的 JSONL 持久化：append-only、崩溃安全、可 resume。
// 文件位于 $XDG_DATA_HOME/tokencode/sessions/YYYY/MM/DD/<id>.jsonl
// （未设 XDG 时 ~/.local/share/...），id 内嵌日期所以由 id 可直接定位文件。
// 每行一个记录：首行 meta，其后每行一条消息。被 agent 剔除的拍不会写入
// （水位线逻辑在 agent 侧），所以文件内容恒等于"留下来的历史"。
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
)

// ErrNotFound 表示找不到匹配的会话。
var ErrNotFound = errors.New("session: not found")

// Meta 是会话元信息（文件首行）。
type Meta struct {
	ID      string `json:"id"`
	Cwd     string `json:"cwd"`
	Model   string `json:"model"`
	Created string `json:"created"` // RFC3339
}

// rec 是一行记录的线上形态。消息字段用稳定的 json tag，不直接序列化
// llm.Message——IR 演进不应破坏已落盘的会话。
type rec struct {
	Type string `json:"type"` // "meta" | "message"

	// meta
	*Meta `json:",omitempty"`

	// message
	Role        string       `json:"role,omitempty"`
	Text        string       `json:"text,omitempty"`
	ToolUses    []toolUse    `json:"tool_uses,omitempty"`
	ToolResults []toolResult `json:"tool_results,omitempty"`
}

type toolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type toolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Dir 返回会话根目录。
func Dir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "tokencode", "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "tokencode", "sessions")
}

// Store 是一个打开的会话文件，只追加。
type Store struct {
	id     string
	f      *os.File
	failed bool // 首次写失败后静默停写：持久化绝不打断对话
}

// ID 返回会话 id。
func (s *Store) ID() string { return s.id }

// Close 关闭文件。
func (s *Store) Close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

// Create 新建会话：id = 时间戳+随机后缀（内嵌日期，可反推路径），写入 meta 行。
func Create(cwd, model string) (*Store, error) {
	now := time.Now()
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	id := now.Format("2006-01-02-150405") + "-" + hex.EncodeToString(suffix)

	path := pathForID(id)
	if path == "" {
		return nil, fmt.Errorf("session: 无法定位数据目录")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{id: id, f: f}
	meta := Meta{ID: id, Cwd: cwd, Model: model, Created: now.Format(time.RFC3339)}
	if err := s.writeLine(rec{Type: "meta", Meta: &meta}); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// Open 以追加模式打开既有会话（resume 后继续写同一个文件）。
func Open(id string) (*Store, error) {
	path := pathForID(id)
	if path == "" {
		return nil, ErrNotFound
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Store{id: id, f: f}, nil
}

// Append 追加一批消息。写失败只记一次状态、不再报错——持久化是尽力而为，
// 绝不让磁盘问题打断正在进行的对话。
func (s *Store) Append(ms []llm.Message) {
	if s.failed {
		return
	}
	for _, m := range ms {
		r := rec{Type: "message", Role: m.Role, Text: m.Text}
		for _, tu := range m.ToolUses {
			r.ToolUses = append(r.ToolUses, toolUse(tu))
		}
		for _, tr := range m.ToolResults {
			r.ToolResults = append(r.ToolResults, toolResult(tr))
		}
		if err := s.writeLine(r); err != nil {
			s.failed = true
			return
		}
	}
}

func (s *Store) writeLine(r rec) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.f.Write(append(b, '\n'))
	return err
}

// Load 按 id 读回会话：重放 JSONL 重建消息序列。损坏的行跳过（崩溃时
// 最后一行可能写了一半），已读到的部分照常返回。
func Load(id string) (Meta, []llm.Message, error) {
	path := pathForID(id)
	if path == "" {
		return Meta{}, nil, ErrNotFound
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Meta{}, nil, ErrNotFound
	}
	if err != nil {
		return Meta{}, nil, err
	}
	defer f.Close()

	var meta Meta
	var msgs []llm.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var r rec
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		switch r.Type {
		case "meta":
			if r.Meta != nil {
				meta = *r.Meta
			}
		case "message":
			m := llm.Message{Role: r.Role, Text: r.Text}
			for _, tu := range r.ToolUses {
				m.ToolUses = append(m.ToolUses, llm.ToolUse(tu))
			}
			for _, tr := range r.ToolResults {
				m.ToolResults = append(m.ToolResults, llm.ToolResult(tr))
			}
			msgs = append(msgs, m)
		}
	}
	return meta, msgs, sc.Err()
}

// Latest 返回 cwd 下最近一个会话的 id（按 id 即时间倒序，读各文件首行比对 cwd）。
func Latest(cwd string) (string, error) {
	root := Dir()
	var ids []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		ids = append(ids, strings.TrimSuffix(d.Name(), ".jsonl"))
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // id 以时间开头，字典序即时间序
	for _, id := range ids {
		meta, err := readMeta(pathForID(id))
		if err == nil && meta.Cwd == cwd {
			return id, nil
		}
	}
	return "", ErrNotFound
}

// readMeta 只读文件首行的 meta。
func readMeta(path string) (Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() {
		return Meta{}, fmt.Errorf("session: 空文件")
	}
	var r rec
	if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Type != "meta" || r.Meta == nil {
		return Meta{}, fmt.Errorf("session: 首行不是 meta")
	}
	return *r.Meta, nil
}

// pathForID 由 id（形如 2026-06-10-150405-ab12cd）反推文件路径。
func pathForID(id string) string {
	root := Dir()
	if root == "" || len(id) < 10 {
		return ""
	}
	y, m, d := id[0:4], id[5:7], id[8:10]
	return filepath.Join(root, y, m, d, id+".jsonl")
}
