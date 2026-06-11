package webui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/channel"
	"github.com/yzfly/tokencode/internal/usage"
)

// usageResponse 是 GET /api/usage 的响应：回显归一后的区间 + 聚合结果。
// from/to 都是日期（含 to 当天，[from, to+1d) 喂给 Summarize）。
type usageResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	usage.Summary
}

// handleUsageAPI 实现 GET /api/usage?from=2006-01-02&to=2006-01-02。
// 缺省区间 = 近 30 天（含今天）；大盘页用两次 fetch 拼出「本月 + 近 30 天」。
func (s *Server) handleUsageAPI(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	from, to := today.AddDate(0, 0, -29), today

	var err error
	if q := r.URL.Query().Get("from"); q != "" {
		if from, err = time.ParseInLocation("2006-01-02", q, now.Location()); err != nil {
			writeError(w, http.StatusBadRequest, "from 不是合法日期（要 2006-01-02）: "+err.Error())
			return
		}
	}
	if q := r.URL.Query().Get("to"); q != "" {
		if to, err = time.ParseInLocation("2006-01-02", q, now.Location()); err != nil {
			writeError(w, http.StatusBadRequest, "to 不是合法日期（要 2006-01-02）: "+err.Error())
			return
		}
	}
	if to.Before(from) {
		writeError(w, http.StatusBadRequest, "to 不能早于 from")
		return
	}

	sum, err := usage.Summarize(from, to.AddDate(0, 0, 1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usageResponse{
		From: from.Format("2006-01-02"), To: to.Format("2006-01-02"), Summary: sum,
	})
}

// pairRequest 是 POST /api/team/pair 的请求体（语义与 `tokencode team pair` 一致）。
type pairRequest struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	Tools     string `json:"tools"` // 逗号分隔白名单；空=默认只读集
	Model     string `json:"model"`
	Yolo      bool   `json:"yolo"`
}

// handleTeamPair 生成一个配对码（写 team.json 的 pending 段，serve 的 IM
// 路由收到陌生用户发码后完成真正绑定）。仅回环可调（requireLocalWrite）。
func (s *Server) handleTeamPair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Workspace) == "" {
		writeError(w, http.StatusBadRequest, "workspace 必填")
		return
	}
	abs, err := filepath.Abs(req.Workspace)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		writeError(w, http.StatusBadRequest, "workspace "+abs+" 不存在或不是目录")
		return
	}
	code, err := channel.GenCode()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := channel.Pending{
		Code:         code,
		Name:         strings.TrimSpace(req.Name),
		Workspace:    abs,
		AllowedTools: splitTools(req.Tools),
		Yolo:         req.Yolo,
		Model:        strings.TrimSpace(req.Model),
		ExpiresAt:    time.Now().Add(channel.PairTTL),
	}
	if err := s.store().AddPending(p); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code": code, "workspace": abs, "expires_at": p.ExpiresAt,
	})
}

// handleTeamUnbind 解除一条绑定：DELETE /api/team/binding?channel=&user_id=。
func (s *Server) handleTeamUnbind(w http.ResponseWriter, r *http.Request) {
	ch := strings.TrimSpace(r.URL.Query().Get("channel"))
	uid := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if ch == "" || uid == "" {
		writeError(w, http.StatusBadRequest, "channel 与 user_id 必填")
		return
	}
	removed, err := s.store().Remove(ch, uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "绑定不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// splitTools 把逗号分隔的白名单拆成切片（空串 → nil = 默认只读集）。
func splitTools(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
