package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 联网工具：websearch（DuckDuckGo HTML 端点，免 key 免费）+ webfetch（取网页转纯文本）。
// 走标准库默认代理逻辑（HTTPS_PROXY 等环境变量），不引第三方依赖。

const (
	webUserAgent   = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	fetchBodyLimit = 2 << 20 // 原始响应读取上限
	fetchTextLimit = 40000   // 喂回模型的文本上限（字符）
	searchMax      = 10
)

// searchResult 是一条搜索结果（引擎无关）。
type searchResult struct {
	Title, URL, Snippet string
}

// searchBackend 是一个免费搜索引擎后端：拼 URL + 解析结果页。
// 引擎们对数据中心 IP 的态度阴晴不定（DDG 会丢 202 挑战），
// 所以按序尝试，谁先给出结果用谁。
type searchBackend struct {
	name  string
	url   func(base, query string) string
	parse func(body string) []searchResult
	base  string // 端点；测试时可替换
}

type webSearchTool struct {
	backends []searchBackend
	client   *http.Client
}

// WebSearch 返回联网搜索工具（DuckDuckGo → Mojeek 依次回退，均无需 API key）。
func WebSearch() Tool {
	return &webSearchTool{
		backends: []searchBackend{ddgBackend(""), mojeekBackend("")},
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ddgBackend 是 DuckDuckGo HTML 版（base 为空用官方端点）。
func ddgBackend(base string) searchBackend {
	if base == "" {
		base = "https://html.duckduckgo.com/html/"
	}
	return searchBackend{
		name: "duckduckgo",
		base: base,
		url:  func(base, q string) string { return base + "?q=" + url.QueryEscape(q) },
		parse: func(body string) []searchResult {
			links := reDDGResult.FindAllStringSubmatch(body, -1)
			snippets := reDDGSnippet.FindAllStringSubmatch(body, -1)
			out := make([]searchResult, 0, len(links))
			for i, m := range links {
				r := searchResult{Title: cleanInline(m[2]), URL: decodeDDGLink(m[1])}
				if i < len(snippets) {
					r.Snippet = cleanInline(snippets[i][1])
				}
				out = append(out, r)
			}
			return out
		},
	}
}

// mojeekBackend 是 Mojeek（独立索引，对自动访问宽容）。
func mojeekBackend(base string) searchBackend {
	if base == "" {
		base = "https://www.mojeek.com/search"
	}
	return searchBackend{
		name: "mojeek",
		base: base,
		url:  func(base, q string) string { return base + "?q=" + url.QueryEscape(q) },
		parse: func(body string) []searchResult {
			out := []searchResult{}
			for _, m := range reMojeek.FindAllStringSubmatch(body, -1) {
				out = append(out, searchResult{
					Title:   cleanInline(m[2]),
					URL:     html.UnescapeString(m[1]),
					Snippet: cleanInline(m[3]),
				})
			}
			return out
		},
	}
}

func (*webSearchTool) Name() string { return "websearch" }

// Concurrent 标记并行安全：纯只读网络请求。
func (*webSearchTool) Concurrent() bool { return true }

func (*webSearchTool) Description() string {
	return "Search the web (DuckDuckGo). Returns titles, URLs and snippets. Use webfetch to read a result page."
}

func (*webSearchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Search query"},
			"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Max results (default 5, max %d)", searchMax)},
		},
		"required": []string{"query"},
	}
}

// 各引擎结果页的抽取正则。都是老式服务端渲染页面，正则抽取足够稳：
//   - DDG：result__a 是标题链接（href 经 uddg 重定向编码），result__snippet 是摘要。
//   - Mojeek：<h2><a class="title" href=URL>标题</a></h2><p class="s">摘要</p>。
var (
	reDDGResult  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reDDGSnippet = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	reMojeek     = regexp.MustCompile(`(?s)<h2><a class="title"[^>]+href="([^"]+)"[^>]*>(.*?)</a></h2>\s*(?:<p class="s">(.*?)</p>)?`)
	reTag        = regexp.MustCompile(`<[^>]*>`)
	reScript     = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>|<head[^>]*>.*?</head>`)
	reBlank      = regexp.MustCompile(`\n{3,}`)
)

func (t *webSearchTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > searchMax {
		limit = searchMax
	}

	var failures []string
	for _, be := range t.backends {
		results, err := t.searchOne(ctx, be, a.Query)
		if err != nil {
			failures = append(failures, be.name+": "+err.Error())
			continue
		}
		if len(results) == 0 {
			failures = append(failures, be.name+": 无结果")
			continue
		}
		var b strings.Builder
		for i, r := range results {
			if i >= limit {
				break
			}
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
			if r.Snippet != "" {
				fmt.Fprintf(&b, "   %s\n", r.Snippet)
			}
		}
		fmt.Fprintf(&b, "（引擎：%s）", be.name)
		return b.String(), nil
	}
	return "", fmt.Errorf("websearch: 所有搜索引擎都失败了 · %s", strings.Join(failures, " · "))
}

// searchOne 查询一个后端：非 200（如 DDG 的 202 反爬挑战）视为该后端失败。
func (t *webSearchTool) searchOne(ctx context.Context, be searchBackend, query string) ([]searchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, be.url(be.base, query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchBodyLimit))
	if err != nil {
		return nil, err
	}
	return be.parse(string(body)), nil
}

// decodeDDGLink 把 DDG 的重定向链接（//duckduckgo.com/l/?uddg=<url 编码>）还原成真实 URL。
func decodeDDGLink(href string) string {
	u, err := url.Parse(html.UnescapeString(href))
	if err != nil {
		return href
	}
	if real := u.Query().Get("uddg"); real != "" {
		return real
	}
	if u.Scheme == "" && strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

type webFetchTool struct {
	client *http.Client
}

// WebFetch 返回网页抓取工具：取 URL 内容并转成可读纯文本。
func WebFetch() Tool {
	return &webFetchTool{client: &http.Client{Timeout: 30 * time.Second}}
}

func (*webFetchTool) Name() string { return "webfetch" }

// Concurrent 标记并行安全：纯只读网络请求。
func (*webFetchTool) Concurrent() bool { return true }

func (*webFetchTool) Description() string {
	return "Fetch a URL and return its content as readable plain text (HTML is stripped). Use after websearch to read pages."
}

func (*webFetchTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "The http(s) URL to fetch"},
		},
		"required": []string{"url"},
	}
}

func (t *webFetchTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("需要 http(s) URL，收到 %q", a.URL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchBodyLimit))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("[HTTP %s]\n%s", resp.Status, truncateText(string(body), 2000)), nil
	}

	text := string(body)
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "html") ||
		(ct == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")) {
		text = htmlToText(text)
	}
	return truncateText(strings.TrimSpace(text), fetchTextLimit), nil
}

// htmlToText 把 HTML 压成可读纯文本：去 script/style、块级标签换行、
// 去标签、解实体、压空行。不是浏览器，但对"读一页资料"足够。
func htmlToText(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	for _, tag := range []string{"</p>", "</div>", "</li>", "</tr>", "</h1>", "</h2>", "</h3>", "</h4>", "<br>", "<br/>", "<br />"} {
		s = strings.ReplaceAll(s, tag, tag+"\n")
	}
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// 逐行修剪，丢掉纯空白行的堆积。
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

// cleanInline 清理行内 HTML 片段（标题/摘要）：去标签、解实体、并空白。
func cleanInline(s string) string {
	s = reTag.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

func truncateText(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	return s[:budget] + fmt.Sprintf("\n...[已截断，原文 %d 字符]", len(s))
}
