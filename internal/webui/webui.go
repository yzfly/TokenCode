// Package webui 是 tokencode serve 自带的使用与管理界面（v0）：
// 纯 html/template + 原生 JS + go:embed，零前端框架、零 CDN（内网可用），
// 单二进制不破功。页面四个——/ui 用量大盘、/ui/chat 极简聊天、
// /ui/team 团队管理、/ui/models 模型目录与凭据状态。
//
// 安全边界沿用 serve：v0 无鉴权，默认仅回环。/api/* 的写操作再加两道闸
// （回环 RemoteAddr + 本机 Host），见 requireLocalWrite。
package webui

import (
	"embed"
	"html/template"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/auth"
	"github.com/yzfly/tokencode/internal/catalog"
	"github.com/yzfly/tokencode/internal/channel"
)

//go:embed templates/*.html
var tmplFS embed.FS

// tmpl 解析全部页面模板（编译期 embed，启动即失败优于运行时 500）。
var tmpl = template.Must(template.ParseFS(tmplFS, "templates/*.html"))

// Server 是 WebUI 的装配点：serve 经 Mount 回调把这里的路由挂上自己的 mux。
type Server struct {
	Version string
	Team    *channel.Store // 团队绑定存储；nil 时首次使用懒初始化为默认路径
}

// Register 把全部 WebUI 路由注册到 mux 上（页面 + JSON API）。
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui", s.handleIndex)
	mux.HandleFunc("GET /ui/chat", s.handleChat)
	mux.HandleFunc("GET /ui/team", s.handleTeam)
	mux.HandleFunc("GET /ui/models", s.handleModels)
	mux.HandleFunc("GET /api/usage", s.handleUsageAPI)
	mux.HandleFunc("POST /api/team/pair", s.requireLocalWrite(s.handleTeamPair))
	mux.HandleFunc("DELETE /api/team/binding", s.requireLocalWrite(s.handleTeamUnbind))
}

// store 返回团队存储（懒初始化默认路径，与 CLI/serve 同一份 team.json）。
func (s *Server) store() *channel.Store {
	if s.Team == nil {
		s.Team = channel.NewStore("")
	}
	return s.Team
}

// requireLocalWrite 是写操作的双重闸：
//  1. RemoteAddr 必须是回环 IP——serve 默认只绑 127.0.0.1，这是用户改绑
//     非回环地址后的第二道防线（页面照常可看，写操作仍只许本机发起）；
//  2. Host 必须是 localhost / 127.0.0.1 / ::1——防 DNS rebinding：恶意网页
//     把自己的域名解析到 127.0.0.1 后，受害者浏览器会带着该域名的 Host 头
//     直打本机端口，此时 RemoteAddr 是回环但 Host 是攻击者域名，校验 Host
//     即可拦截（v0 无鉴权下这是最廉价的有效防御）。
func (s *Server) requireLocalWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			writeError(w, http.StatusForbidden, "写操作仅接受回环来源")
			return
		}
		if !isLocalHostname(r.Host) {
			writeError(w, http.StatusForbidden, "Host 非本机（疑似 DNS rebinding），拒绝")
			return
		}
		next(w, r)
	}
}

// isLocalHostname 报告 Host 头（可带端口）是否指向本机。
func isLocalHostname(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// ---- 页面 ----

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.render(w, "index", map[string]any{"Version": s.Version})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.render(w, "chat", map[string]any{"Version": s.Version})
}

// teamPageData 是 /ui/team 的模板数据。
type teamPageData struct {
	Version  string
	Err      string
	Bindings []channel.Binding
	Pending  []channel.Pending
}

func (s *Server) handleTeam(w http.ResponseWriter, r *http.Request) {
	data := teamPageData{Version: s.Version}
	t, err := s.store().Load()
	if err != nil {
		data.Err = err.Error()
	}
	data.Bindings = t.Bindings
	// 过期 pending 只在展示层过滤（落盘清理由 AddPending/Pair 顺带做）。
	now := time.Now()
	for _, p := range t.Pending {
		if now.Before(p.ExpiresAt) {
			data.Pending = append(data.Pending, p)
		}
	}
	s.render(w, "team", data)
}

// providerRow 是 /ui/models 表格的一行。
type providerRow struct {
	ID       string
	Name     string
	Protocol string
	BaseURL  string
	Models   int
	Cred     string // "✓ env (VAR)" / "✓ auth.json" / "—"
	HasCred  bool
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	creds, _ := auth.Load() // 读失败按无凭据展示（只读页，不报 500）
	var rows []providerRow
	for _, p := range catalog.All() {
		row := providerRow{
			ID: p.ID, Name: p.Name, Protocol: p.Protocol,
			BaseURL: p.BaseURL, Models: len(p.Models), Cred: "—",
		}
		if env := credEnvName(p); env != "" {
			row.Cred, row.HasCred = "✓ env ("+env+")", true
		} else if creds[p.ID].Key != "" {
			row.Cred, row.HasCred = "✓ auth.json", true
		}
		rows = append(rows, row)
	}
	// 有凭据的排前面（这才是「我的」provider），其余保持目录字典序。
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].HasCred && !rows[j].HasCred })
	s.render(w, "models", map[string]any{"Version": s.Version, "Rows": rows})
}

// credEnvName 返回第一个有值的 key 环境变量名（与 Provider.KeyFromEnv 同序）。
func credEnvName(p catalog.Provider) string {
	for _, e := range p.Env {
		if os.Getenv(e) != "" {
			return e
		}
	}
	return ""
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		// 头已发出，只能尽力而为；模板是编译期 embed 的，正常不会走到这。
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
