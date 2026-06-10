// Package auth 管理 provider 凭据：`tokencode auth login` 写入的 key
// 存在 config 同目录的 auth.json（0600）。目录（catalog）给元数据，
// 这里的凭据决定一个 provider 是否可用——与 opencode 的 auth.json 同构，
// 形态上为将来的 OAuth（refresh/access/expires）留位。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Credential 是一个 provider 的凭据。
type Credential struct {
	Type string `json:"type"`          // "api"（现阶段唯一）；为 "oauth" 留位
	Key  string `json:"key,omitempty"` // type=api 的 API key
}

// Path 返回 auth.json 路径（与 config.json 同目录）。
func Path() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tokencode", "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "tokencode", "auth.json")
}

// Load 读取全部凭据。文件不存在返回空表（不是错误）。
func Load() (map[string]Credential, error) {
	p := Path()
	if p == "" {
		return map[string]Credential{}, nil
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Credential{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]Credential
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", p, err)
	}
	if m == nil {
		m = map[string]Credential{}
	}
	return m, nil
}

// Get 取一个 provider 的凭据 key（无则空串）。读文件失败按无凭据处理。
func Get(provider string) string {
	m, err := Load()
	if err != nil {
		return ""
	}
	return m[provider].Key
}

// Set 写入/覆盖一个 provider 的凭据并落盘（0600）。
func Set(provider string, c Credential) error {
	m, err := Load()
	if err != nil {
		return err
	}
	m[provider] = c
	return save(m)
}

// Remove 删除一个 provider 的凭据。不存在不是错误。
func Remove(provider string) error {
	m, err := Load()
	if err != nil {
		return err
	}
	if _, ok := m[provider]; !ok {
		return nil
	}
	delete(m, provider)
	return save(m)
}

// Providers 返回已存凭据的 provider 名（排序）。
func Providers() []string {
	m, _ := Load()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// save 原子落盘：临时文件 + rename，权限 0600。
func save(m map[string]Credential) error {
	p := Path()
	if p == "" {
		return fmt.Errorf("auth: 无法定位配置目录")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
