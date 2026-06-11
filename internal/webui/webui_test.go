package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/channel"
	"github.com/yzfly/tokencode/internal/usage"
)

// newTestServer 起一个 webui mux：usage/auth 数据目录都指到临时位置，
// team.json 用独立临时文件——绝不碰真实账本与配置。
func newTestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := &Server{Version: "test", Team: channel.NewStore(filepath.Join(t.TempDir(), "team.json"))}
	mux := http.NewServeMux()
	s.Register(mux)
	return s, mux
}

// localReq 构造一个「合法本机写请求」：回环 RemoteAddr + localhost Host。
func localReq(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = "127.0.0.1:8787"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestIndexPage(t *testing.T) {
	_, mux := newTestServer(t)
	rec := get(t, mux, "/ui")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"用量大盘", "近 30 天", "/api/usage", "按模型", "按来源", "勿绑 0.0.0.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("/ui 缺少关键元素 %q", want)
		}
	}
}

func TestChatAndModelsPages(t *testing.T) {
	_, mux := newTestServer(t)
	if rec := get(t, mux, "/ui/chat"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "/v1/run") {
		t.Fatalf("GET /ui/chat = %d，或缺 /v1/run 引用", rec.Code)
	}
	rec := get(t, mux, "/ui/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/models = %d", rec.Code)
	}
	// 内置目录必含 anthropic 条目；本测试环境未配凭据时显示「—」也合法，
	// 只验证页面骨架与目录确实渲染了。
	if body := rec.Body.String(); !strings.Contains(body, "anthropic") ||
		!strings.Contains(body, "凭据") {
		t.Error("/ui/models 缺 provider 目录或凭据列")
	}
}

func TestUsageAPI(t *testing.T) {
	_, mux := newTestServer(t)
	// 喂一个临时账本：今天两条、区间外（40 天前）一条。
	now := time.Now()
	dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "tokencode", "usage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rec usage.Record) {
		b, _ := json.Marshal(rec)
		path := filepath.Join(dir, rec.TS.Format("2006-01")+".jsonl")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		f.Write(append(b, '\n'))
	}
	write(usage.Record{TS: now, Model: "m1", Source: "user", In: 100, Out: 10, CacheRead: 5})
	write(usage.Record{TS: now, Model: "m2", Source: "serve", In: 200, Out: 20, CacheWrite: 7})
	write(usage.Record{TS: now.AddDate(0, 0, -40), Model: "m1", Source: "user", In: 999, Out: 999})

	day := now.Format("2006-01-02")
	rec := get(t, mux, "/api/usage?from="+day+"&to="+day)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/usage = %d: %s", rec.Code, rec.Body.String())
	}
	var got usageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.From != day || got.To != day {
		t.Errorf("区间回显 = %s..%s，要 %s..%s", got.From, got.To, day, day)
	}
	if got.Total.In != 300 || got.Total.Out != 30 || got.Total.Calls != 2 ||
		got.Total.CacheRead != 5 || got.Total.CacheWrite != 7 {
		t.Errorf("total = %+v，区间外记录漏滤或聚合错", got.Total)
	}
	if got.ByModel["m1"].In != 100 || got.ByModel["m2"].In != 200 {
		t.Errorf("by_model = %+v", got.ByModel)
	}
	if got.BySource["serve"].Calls != 1 {
		t.Errorf("by_source = %+v", got.BySource)
	}
	if got.ByDay[day].Calls != 2 {
		t.Errorf("by_day = %+v", got.ByDay)
	}

	// 坏日期 → 400。
	if rec := get(t, mux, "/api/usage?from=昨天"); rec.Code != http.StatusBadRequest {
		t.Errorf("坏 from = %d，要 400", rec.Code)
	}
}

func TestTeamPairAndUnbind(t *testing.T) {
	s, mux := newTestServer(t)
	ws := t.TempDir()

	// 1. 生成配对码。
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("POST", "/api/team/pair",
		`{"workspace":`+strconv(ws)+`,"name":"小明","tools":"read, bash"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("pair = %d: %s", rec.Code, rec.Body.String())
	}
	var pr struct{ Code string }
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil || len(pr.Code) != 8 {
		t.Fatalf("配对码异常: %s (err=%v)", rec.Body.String(), err)
	}
	team, _ := s.store().Load()
	if len(team.Pending) != 1 || team.Pending[0].Workspace != ws ||
		len(team.Pending[0].AllowedTools) != 2 {
		t.Fatalf("pending 落盘异常: %+v", team.Pending)
	}

	// 2. 团队页能看到 pending；认领后能看到绑定。
	if body := get(t, mux, "/ui/team").Body.String(); !strings.Contains(body, pr.Code) {
		t.Error("/ui/team 不含刚生成的配对码")
	}
	if _, ok, err := s.store().Pair("feishu", "u1", "小明", pr.Code); err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	if body := get(t, mux, "/ui/team").Body.String(); !strings.Contains(body, "u1") ||
		!strings.Contains(body, "小明") {
		t.Error("/ui/team 不含绑定行")
	}

	// 3. 解绑：先打不存在的 → 404，再打真的 → 200 且落盘删除。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("DELETE", "/api/team/binding?channel=feishu&user_id=没这人", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("解绑不存在者 = %d，要 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("DELETE", "/api/team/binding?channel=feishu&user_id=u1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("解绑 = %d: %s", rec.Code, rec.Body.String())
	}
	if team, _ := s.store().Load(); len(team.Bindings) != 0 {
		t.Errorf("解绑后仍有绑定: %+v", team.Bindings)
	}

	// 4. 坏 workspace → 400。
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, localReq("POST", "/api/team/pair", `{"workspace":"/不存在/的/目录"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("坏 workspace = %d，要 400", rec.Code)
	}
}

// TestWriteRequiresLoopback 验证写操作的双重闸：非回环 RemoteAddr 与
// 非本机 Host（DNS rebinding 形态）都得 403，且不产生任何副作用。
func TestWriteRequiresLoopback(t *testing.T) {
	s, mux := newTestServer(t)
	ws := t.TempDir()
	body := `{"workspace":` + strconv(ws) + `}`

	// httptest.NewRequest 默认 RemoteAddr=192.0.2.1（非回环）、Host=example.com。
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/team/pair", strings.NewReader(body)))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非回环 pair = %d，要 403", rec.Code)
	}

	// 回环来源但 Host 是外域（DNS rebinding 的浏览器形态）→ 403。
	r := httptest.NewRequest("POST", "/api/team/pair", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "evil.example.com"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Errorf("rebinding pair = %d，要 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/team/binding?channel=a&user_id=b", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非回环解绑 = %d，要 403", rec.Code)
	}

	if team, _ := s.store().Load(); len(team.Pending) != 0 {
		t.Errorf("403 请求竟产生副作用: %+v", team.Pending)
	}
}

// strconv 把字符串安全编码成 JSON 字面量（路径里可能有反斜杠等）。
func strconv(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
