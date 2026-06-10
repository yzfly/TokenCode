package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ddgPage 是 DuckDuckGo HTML 版结果页的最小仿真。
const ddgPage = `<html><body>
<div class="result">
  <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=x">Go <b>Documentation</b></a>
  <a class="result__snippet" href="#">The Go programming &amp; language docs.</a>
</div>
<div class="result">
  <a rel="nofollow" class="result__a" href="https://example.com/direct">Direct Link</a>
  <a class="result__snippet" href="#">Second snippet</a>
</div>
</body></html>`

// mojeekPage 是 Mojeek 结果页的最小仿真。
const mojeekPage = `<ul><li class="r1"><a href="https://go.dev/" class="ob"></a>
<h2><a class="title" title="https://go.dev/" href="https://go.dev/">The Go Programming Language</a></h2>
<p class="s">Go is an open source language.</p></li></ul>`

func TestWebSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); q != "golang docs" {
			t.Errorf("query = %q", q)
		}
		w.Write([]byte(ddgPage))
	}))
	defer srv.Close()

	tool := &webSearchTool{backends: []searchBackend{ddgBackend(srv.URL)}, client: &http.Client{Timeout: 5 * time.Second}}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang docs"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go Documentation", "https://go.dev/doc/", "language docs", "https://example.com/direct"} {
		if !strings.Contains(out, want) {
			t.Errorf("search output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<b>") || strings.Contains(out, "uddg") {
		t.Errorf("output should be clean text with decoded links:\n%s", out)
	}

	// limit 生效。
	out, err = tool.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang docs", "limit": 1}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Direct Link") {
		t.Errorf("limit=1 should drop second result:\n%s", out)
	}
}

func TestWebSearchFallback(t *testing.T) {
	// DDG 仿真返回 202 反爬挑战 → 自动回退 Mojeek。
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer blocked.Close()
	moj := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(mojeekPage))
	}))
	defer moj.Close()

	tool := &webSearchTool{
		backends: []searchBackend{ddgBackend(blocked.URL), mojeekBackend(moj.URL)},
		client:   &http.Client{Timeout: 5 * time.Second},
	}
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The Go Programming Language", "https://go.dev/", "open source language", "引擎：mojeek"} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback output missing %q:\n%s", want, out)
		}
	}

	// 两个后端都挂 → 报错并点名。
	tool.backends = []searchBackend{ddgBackend(blocked.URL), mojeekBackend(blocked.URL)}
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang"})); err == nil ||
		!strings.Contains(err.Error(), "duckduckgo") || !strings.Contains(err.Error(), "mojeek") {
		t.Errorf("all-fail should name both backends, got: %v", err)
	}
}

// TestWebSearchLive 打真实引擎（默认跳过；TOKENCODE_LIVE_WEB_TEST=1 开启）。
// 引擎对数据中心 IP 的态度会变，这个测试用来现场验证回退链还活着。
func TestWebSearchLive(t *testing.T) {
	if os.Getenv("TOKENCODE_LIVE_WEB_TEST") == "" {
		t.Skip("set TOKENCODE_LIVE_WEB_TEST=1 to run against real engines")
	}
	out, err := WebSearch().Execute(context.Background(), mustJSON(t, map[string]any{"query": "golang context cancellation"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "http") {
		t.Errorf("live search returned no links:\n%s", out)
	}
	t.Logf("live result:\n%s", out)
}

func TestWebFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>t</title><style>body{color:red}</style></head>
<body><script>evil()</script><h1>Hello</h1><p>World &amp; peace</p></body></html>`))
	}))
	defer srv.Close()

	tool := WebFetch()
	out, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"url": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "World & peace") {
		t.Errorf("fetch output missing content:\n%s", out)
	}
	if strings.Contains(out, "evil()") || strings.Contains(out, "color:red") {
		t.Errorf("script/style should be stripped:\n%s", out)
	}

	// 非 http(s) 拒绝。
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{"url": "file:///etc/passwd"})); err == nil {
		t.Error("non-http URL should be rejected")
	}
}
