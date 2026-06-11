// Package config 是 TokenCode 的模型注册表：providers（端点）+ models（别名）。
// 只读一处文件：$XDG_CONFIG_HOME/tokencode/config.json（未设 XDG 时 ~/.config/tokencode/config.json）。
// 文件不存在不是错误——零值 config 下行为与无 config 时代完全一致。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yzfly/tokencode/internal/auth"
	"github.com/yzfly/tokencode/internal/catalog"
	"github.com/yzfly/tokencode/internal/mcp"
)

// 协议类型。每种协议对应 internal/llm 里的一个 codec，名字就叫协议名。
const (
	ProtocolAnthropic = "anthropic"
	ProtocolOpenAI    = "openai"
	ProtocolGoogle    = "google"

	// ProtocolOpenAIChat 是 "openai" 的旧称，Load 时静默归一化，仅为兼容已有 config 保留。
	ProtocolOpenAIChat = "openai-chat"
)

// normalizeProtocol 把旧协议名归一到现行名。
func normalizeProtocol(p string) string {
	if p == ProtocolOpenAIChat {
		return ProtocolOpenAI
	}
	return p
}

// Provider 是一个端点条目：baseURL + 协议 + 鉴权。纯配置，不是代码。
type Provider struct {
	BaseURL   string `json:"base_url"`
	Protocol  string `json:"protocol"`    // "anthropic" | "openai" | "google"
	APIKey    string `json:"api_key"`     // 直接写 key（不推荐，便于本地试用）
	APIKeyEnv string `json:"api_key_env"` // 从该环境变量读 key（推荐）
	Auth      string `json:"auth"`        // anthropic 协议下："bearer" | "x-api-key"（默认）
}

// Config 是配置文件的全貌。
type Config struct {
	Providers    map[string]Provider         `json:"providers"`
	Models       map[string]string           `json:"models"` // 别名 → "provider/model-id"
	DefaultModel string                      `json:"default_model"`
	AutoModel    string                      `json:"auto_model"` // auto 模式权限裁决用的小模型（别名或 provider/model-id）；空=用主模型
	MCP          map[string]mcp.ServerConfig `json:"mcp"`        // MCP server 名 → stdio 配置
	Race         RaceConfig                  `json:"race"`       // 并行竞赛模式（/race）
	Channels     ChannelsConfig              `json:"channels"`   // IM 通道（团队模式，serve 时启用）
}

// ChannelsConfig 是各 IM 通道的接入凭据。纯配置：adapter 实现在 internal/channel 下。
type ChannelsConfig struct {
	Feishu FeishuChannel `json:"feishu"`
	Wechat WechatChannel `json:"wechat"`
}

// FeishuChannel 是飞书自建应用凭据（长连接接入，免公网 IP）。
type FeishuChannel struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// Enabled 报告飞书通道是否配置完整。
func (f FeishuChannel) Enabled() bool { return f.AppID != "" && f.AppSecret != "" }

// WechatChannel 是微信 iLink Bot 通道（实验性，DM-only）。凭证不在 config 里
// ——成员各自 `tokencode wechat login` 扫码落盘；这里只有显式开关与基座覆盖。
type WechatChannel struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"` // 空用官方默认基座（协议灰度期可覆盖）
}

// RaceConfig 是竞赛模式的可调参数。
type RaceConfig struct {
	Concurrency int    `json:"concurrency"` // 同时在飞的 racer 窗口；≤0 用内置默认（8）
	Check       string `json:"check"`       // 客观校验命令（淘汰用，在各 worktree 内跑）；空=跳过
}

// Target 是 -model 解析后的落点：构造 llm 客户端所需的全部信息。
type Target struct {
	Protocol string // "anthropic" | "openai" | "google"
	BaseURL  string
	APIKey   string
	Bearer   bool   // anthropic 协议下是否用 Authorization: Bearer
	Model    string // 实际发给端点的 model id
	Default  bool   // true 表示未命中 config，走默认 provider（调用方按现状兜底）
}

// Path 返回配置文件路径。
func Path() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tokencode", "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "tokencode", "config.json")
}

// Load 读取配置。文件不存在时返回零值 config（不是错误）。
func Load() (Config, error) {
	p := Path()
	if p == "" {
		return Config{}, nil
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", p, err)
	}
	return c, nil
}

// Resolve 解析 -model 参数。解析顺序：
// ① models 别名 → ② "provider/model-id" 语法（前缀须是已知 provider）→ ③ 原样直传（Default=true）。
func (c Config) Resolve(model string) (Target, error) {
	if full, ok := c.Models[model]; ok {
		t, err := c.split(full)
		if err != nil {
			return Target{}, fmt.Errorf("config: 别名 %q → %q: %w", model, full, err)
		}
		return t, nil
	}
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := c.Providers[model[:i]]; ok {
			return c.split(model)
		}
		// 用户 config 没配的 provider 落到内置目录（embed 的 models.dev 快照）：
		// 国内外模型与 coding plan 无需手写 config 即可用。
		if t, ok, err := resolveCatalog(model[:i], model[i+1:]); ok {
			return t, err
		}
	}
	return Target{Protocol: ProtocolAnthropic, Model: model, Default: true}, nil
}

// resolveCatalog 用内置目录解析 "provider/model-id"。ok=false 表示目录里
// 没有这个 provider（继续走默认直传）；命中但缺 key 时给出带指引的错误。
func resolveCatalog(name, modelID string) (Target, bool, error) {
	p, found := catalog.Find(name)
	if !found || !p.Usable() {
		return Target{}, false, nil
	}
	key := p.KeyFromEnv()
	if key == "" {
		key = auth.Get(p.ID)
	}
	if key == "" {
		hint := strings.Join(p.Env, " 或 ")
		if hint == "" {
			hint = "（该 provider 未声明环境变量）"
		}
		return Target{}, true, fmt.Errorf(
			"provider %q 在内置目录中，但没有可用凭据：运行 `tokencode auth login %s` 或设置环境变量 %s",
			name, name, hint)
	}
	return Target{
		Protocol: p.Protocol,
		BaseURL:  p.BaseURL,
		APIKey:   key,
		// 目录条目走 Authorization: Bearer——anthropic 兼容的 coding plan
		//（Kimi/MiniMax/智谱…）均接受 Bearer 形态的 token。
		Bearer: p.Protocol == ProtocolAnthropic,
		Model:  modelID,
	}, true, nil
}

// split 把 "provider/model-id" 拆开并装配 Target。
func (c Config) split(full string) (Target, error) {
	i := strings.Index(full, "/")
	if i <= 0 || i == len(full)-1 {
		return Target{}, fmt.Errorf("不是 provider/model-id 形式")
	}
	name, modelID := full[:i], full[i+1:]
	p, ok := c.Providers[name]
	if !ok {
		// 别名也可以指向内置目录的 provider（如 "k2": "kimi-for-coding/kimi-…"）。
		if t, found, err := resolveCatalog(name, modelID); found {
			return t, err
		}
		return Target{}, fmt.Errorf("未知 provider %q", name)
	}
	proto := normalizeProtocol(p.Protocol)
	switch proto {
	case ProtocolAnthropic, ProtocolOpenAI, ProtocolGoogle:
	default:
		return Target{}, fmt.Errorf("provider %q: protocol 须为 %q、%q 或 %q", name, ProtocolAnthropic, ProtocolOpenAI, ProtocolGoogle)
	}
	key := p.APIKey
	if key == "" && p.APIKeyEnv != "" {
		key = os.Getenv(p.APIKeyEnv)
	}
	return Target{
		Protocol: proto,
		BaseURL:  p.BaseURL,
		APIKey:   key,
		Bearer:   p.Auth == "bearer",
		Model:    modelID,
	}, nil
}
