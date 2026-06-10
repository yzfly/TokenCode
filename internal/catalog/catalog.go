// Package catalog 是内置的 provider 目录：编译期 embed 一份
// models.dev（https://models.dev，MIT）的裁剪快照，让国内外模型与
// coding plan（Kimi for Coding、智谱 GLM、阿里百炼、MiniMax、DeepSeek…）
// 开箱即用——目录给元数据（端点/协议/key 环境变量），凭据决定可用性。
//
// 更新快照：scripts/update-catalog.sh。
// 运行时覆盖：TOKENCODE_CATALOG 指向本地 JSON 文件（私有镜像/调试用）。
package catalog

import (
	_ "embed"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
)

//go:embed modelsdev.json
var embedded []byte

// Provider 是目录里的一个端点条目。
type Provider struct {
	ID       string
	Name     string
	Protocol string   // 已映射到 TokenCode codec：anthropic / openai / google
	BaseURL  string   // 空 = 协议默认端点（anthropic/google 有默认，openai 必填故空即不可用）
	Env      []string // 自动探测的 key 环境变量
	Doc      string
	Models   []Model // 按 id 排序
}

// Model 是一个模型条目。
type Model struct {
	ID      string
	Name    string
	Context int
}

// raw 对应裁剪快照的 JSON 结构。
type rawProvider struct {
	Name   string              `json:"name"`
	Env    []string            `json:"env"`
	NPM    string              `json:"npm"`
	API    string              `json:"api"`
	Doc    string              `json:"doc"`
	Models map[string]rawModel `json:"models"`
}

type rawModel struct {
	Name    string `json:"name"`
	Context int    `json:"context"`
}

var (
	once      sync.Once
	providers []Provider
	byID      map[string]Provider
)

// load 解析快照（一次）。TOKENCODE_CATALOG 存在且可读时优先。
func load() {
	once.Do(func() {
		data := embedded
		if p := os.Getenv("TOKENCODE_CATALOG"); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				data = b
			}
		}
		var raw map[string]rawProvider
		if err := json.Unmarshal(data, &raw); err != nil {
			byID = map[string]Provider{}
			return
		}
		byID = make(map[string]Provider, len(raw))
		for id, rp := range raw {
			proto := protocolFor(rp.NPM)
			api := rp.API
			// models.dev 的 anthropic 条目沿用 ai-sdk 约定：base 带 /v1、
			// 客户端只补 /messages。TokenCode 的 codec 是 base + /v1/messages，
			// 这里剥掉尾部 /v1 对齐（如 kimi 的 …/coding/v1 → …/coding）。
			if proto == "anthropic" {
				api = strings.TrimSuffix(strings.TrimRight(api, "/"), "/v1")
			}
			p := Provider{
				ID:       id,
				Name:     rp.Name,
				Protocol: proto,
				BaseURL:  api,
				Env:      rp.Env,
				Doc:      rp.Doc,
			}
			for mid, rm := range rp.Models {
				p.Models = append(p.Models, Model{ID: mid, Name: rm.Name, Context: rm.Context})
			}
			sort.Slice(p.Models, func(i, j int) bool { return p.Models[i].ID < p.Models[j].ID })
			byID[id] = p
			providers = append(providers, p)
		}
		sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	})
}

// protocolFor 把 models.dev 的 npm 适配器字段映射到 TokenCode 的协议 codec。
// 不认识的一律按 openai 兼容处理（与 opencode 的兜底策略一致）。
func protocolFor(npm string) string {
	switch {
	case strings.HasPrefix(npm, "@ai-sdk/anthropic"):
		return "anthropic"
	case strings.HasPrefix(npm, "@ai-sdk/google"):
		return "google"
	default:
		return "openai"
	}
}

// All 返回目录全部 provider（按 id 排序）。
func All() []Provider {
	load()
	return providers
}

// Find 按 id 查 provider。
func Find(id string) (Provider, bool) {
	load()
	p, ok := byID[id]
	return p, ok
}

// Usable 报告一个目录条目当下是否可直接构造客户端：
// openai 协议必须有 base_url（无默认端点），其余协议有默认可缺省。
func (p Provider) Usable() bool {
	return p.BaseURL != "" || p.Protocol != "openai"
}

// KeyFromEnv 依声明的环境变量探测 key（第一个非空者）。
func (p Provider) KeyFromEnv() string {
	for _, e := range p.Env {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return ""
}
