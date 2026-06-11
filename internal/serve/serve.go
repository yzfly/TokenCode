// Package serve 实现 tokencode 的 HTTP API 雏形（v0）：每个请求装配一个独立
// agent 跑一个 turn，无共享历史（stateless）。无鉴权——因此默认只绑回环地址，
// 将来加 token 鉴权时在 Handler 外再包一层即可（留位）。
//
// agent 的装配（模型解析、客户端构造、工具注册表）经 Assemble 注入：HTTP 层
// 不依赖 config/catalog，测试用 fake LLM 工厂即可覆盖全部路径。
package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/headless"
)

// Server 是 HTTP API 的装配点。
type Server struct {
	Version       string
	MaxConcurrent int // 同时在跑的 run 上限；≤0 用默认 8
	// Assemble 为一次请求装配独立 agent：model 空=默认模型，返回解析后的
	// 实际 model id。错误一律视为请求问题（未知 model / 缺 key）→ 400。
	Assemble func(model string, allowed []string) (*agent.Agent, string, error)

	once sync.Once
	sem  chan struct{} // 计数信号量：限制同时在跑的 run
}

// runRequest 是 POST /v1/run 的请求体。
type runRequest struct {
	Prompt       string   `json:"prompt"`
	Model        string   `json:"model"`         // 可选；空=服务默认模型
	AllowedTools []string `json:"allowed_tools"` // 可选；缺省 headless.DefaultAllowed
}

// Handler 构造路由。复用同一个 Server 多次调用安全（信号量只建一次）。
func (s *Server) Handler() http.Handler {
	s.once.Do(func() {
		n := s.MaxConcurrent
		if n <= 0 {
			n = 8
		}
		s.sem = make(chan struct{}, n)
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/run", s.handleRun)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.Version})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt 不能为空")
		return
	}
	allowed := req.AllowedTools
	if allowed == nil {
		allowed = headless.DefaultAllowed
	}

	// 信号量满时排队等（而非 429）：v0 客户端少，排队语义最省事；
	// 客户端断开（ctx 取消）即放弃排队。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	ag, model, err := s.Assemble(req.Model, allowed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if wantsStream(r) {
		s.runSSE(w, r, ag, model, req.Prompt)
		return
	}
	res := headless.Run(r.Context(), ag, model, req.Prompt, nil)
	writeJSON(w, http.StatusOK, res)
}

// runSSE 以 Server-Sent Events 推事件流，事件结构与 -output stream-json 完全一致；
// 发完 result 事件即返回（连接由 http.Server 收尾关闭）。
func (s *Server) runSSE(w http.ResponseWriter, r *http.Request, ag *agent.Agent, model, prompt string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "底层连接不支持流式输出")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	headless.Run(r.Context(), ag, model, prompt, func(ev headless.Event) {
		b, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	})
}

// wantsStream 判断请求是否要 SSE：Accept 带 text/event-stream 或 ?stream=1。
func wantsStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream") ||
		r.URL.Query().Get("stream") == "1"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
