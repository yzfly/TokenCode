package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// load 是测试便捷构造：在空临时目录上 Load（无项目级文件），失败即 fatal。
func load(t *testing.T, global Config) *Runner {
	t.Helper()
	r, err := Load(global, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r == nil {
		t.Fatal("Load 返回 nil Runner（配置非空时不该发生）")
	}
	return r
}

func TestNilRunnerFastPath(t *testing.T) {
	var r *Runner
	if block, reason := r.OnPreTool("bash", nil); block || reason != "" {
		t.Fatalf("nil Runner 不该阻断：%v %q", block, reason)
	}
	r.OnPostTool("bash", nil, "out", false) // 不 panic 即可
	r.OnSessionStart()
	r.OnStop()

	// 配置全空时 Load 必须返回 nil（零开销路径的入口）。
	if got, err := Load(nil, t.TempDir()); err != nil || got != nil {
		t.Fatalf("空配置应返回 (nil, nil)，got (%v, %v)", got, err)
	}
}

func TestMatcherFilter(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		tool    string
		want    bool // hook 是否应当执行（用 exit 2 是否阻断来观测）
	}{
		{"精确命中", "bash", "bash", true},
		{"不命中", "bash", "write", false},
		{"空 matcher 全匹配", "", "anything", true},
		{"多选命中", "write|edit", "edit", true},
		{"整名匹配不误中前缀", "write|edit", "editx", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := load(t, Config{EventPreToolUse: {{Matcher: c.matcher, Command: "exit 2"}}})
			block, _ := r.OnPreTool(c.tool, nil)
			if block != c.want {
				t.Fatalf("matcher %q tool %q：block=%v want %v", c.matcher, c.tool, block, c.want)
			}
		})
	}
}

func TestPreToolBlockWithReason(t *testing.T) {
	r := load(t, Config{EventPreToolUse: {{Command: "echo no-go >&2; exit 2"}}})
	var notes []string
	r.Notify = func(s string) { notes = append(notes, s) }

	block, reason := r.OnPreTool("bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if !block || reason != "no-go" {
		t.Fatalf("want 阻断 + 理由 no-go，got block=%v reason=%q", block, reason)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no-go") {
		t.Fatalf("阻断应有 note 提示，got %v", notes)
	}
}

func TestNonZeroExitWarnsWithoutBlocking(t *testing.T) {
	r := load(t, Config{EventPreToolUse: {{Command: "echo broken >&2; exit 1"}}})
	var notes []string
	r.Notify = func(s string) { notes = append(notes, s) }

	block, _ := r.OnPreTool("bash", nil)
	if block {
		t.Fatal("exit 1 不该阻断")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "退出码 1") || !strings.Contains(notes[0], "broken") {
		t.Fatalf("want 含退出码与 stderr 的警告，got %v", notes)
	}
}

func TestStdinPayloadShape(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "payload.json")
	r := load(t, Config{EventPostToolUse: {{Command: fmt.Sprintf("cat > %q", out)}}})

	longResult := strings.Repeat("结", 3000)
	r.OnPostTool("bash", json.RawMessage(`{"command":"ls"}`), longResult, true)

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook 没收到 stdin：%v", err)
	}
	var p struct {
		Event   string          `json:"event"`
		Tool    string          `json:"tool"`
		Input   json.RawMessage `json:"input"`
		Result  string          `json:"result"`
		IsError *bool           `json:"is_error"`
		Cwd     string          `json:"cwd"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("载荷不是合法 JSON：%v\n%s", err, raw)
	}
	if p.Event != "PostToolUse" || p.Tool != "bash" || string(p.Input) != `{"command":"ls"}` {
		t.Fatalf("载荷字段不对：%+v", p)
	}
	if n := len([]rune(p.Result)); n != 2000 {
		t.Fatalf("result 应截断到 2000 字符，got %d", n)
	}
	if p.IsError == nil || !*p.IsError {
		t.Fatalf("is_error 应为 true，got %v", p.IsError)
	}
	if p.Cwd == "" {
		t.Fatal("载荷应带 cwd")
	}
}

func TestPreToolPayloadHasNoResult(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "payload.json")
	r := load(t, Config{EventPreToolUse: {{Command: fmt.Sprintf("cat > %q", out)}}})

	r.OnPreTool("read", json.RawMessage(`{"path":"a.go"}`))
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["result"]; has {
		t.Fatalf("PreToolUse 载荷不该有 result：%s", raw)
	}
	if _, has := m["is_error"]; has {
		t.Fatalf("PreToolUse 载荷不该有 is_error：%s", raw)
	}
}

func TestEnvConvenienceVars(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	r := load(t, Config{EventPostToolUse: {{
		Command: fmt.Sprintf(`printf '%%s|%%s|%%s' "$TOKENCODE_EVENT" "$TOKENCODE_TOOL" "$TOKENCODE_FILE" > %q`, out),
	}}})

	r.OnPostTool("write", json.RawMessage(`{"path":"/tmp/x.go","content":"hi"}`), "ok", false)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "PostToolUse|write|/tmp/x.go"; got != want {
		t.Fatalf("env 便捷字段：got %q want %q", got, want)
	}
}

func TestTimeoutWarnsWithoutBlocking(t *testing.T) {
	r := load(t, Config{EventPreToolUse: {{Command: "sleep 5; exit 2"}}})
	r.Timeout = 100 * time.Millisecond
	var notes []string
	r.Notify = func(s string) { notes = append(notes, s) }

	start := time.Now()
	block, _ := r.OnPreTool("bash", nil)
	if block {
		t.Fatal("超时应按非阻断处理")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时控制失效，耗时 %v", elapsed)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "超时") {
		t.Fatalf("want 超时警告，got %v", notes)
	}
}

func TestSystemMessagePassthrough(t *testing.T) {
	r := load(t, Config{EventStop: {{Command: `echo '{"systemMessage":"hello from hook"}'`}}})
	var notes []string
	r.Notify = func(s string) { notes = append(notes, s) }
	r.OnStop()
	if len(notes) != 1 || notes[0] != "hello from hook" {
		t.Fatalf("systemMessage 应透传为 note，got %v", notes)
	}

	// 普通 stdout 忽略：无 note。
	r2 := load(t, Config{EventStop: {{Command: "echo just some output"}}})
	var notes2 []string
	r2.Notify = func(s string) { notes2 = append(notes2, s) }
	r2.OnStop()
	if len(notes2) != 0 {
		t.Fatalf("非 systemMessage 的 stdout 应忽略，got %v", notes2)
	}
}

func TestLoadMergeProjectFirst(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "order.log")
	proj := Config{EventStop: {{Command: fmt.Sprintf("echo project >> %q", log)}}}
	raw, _ := json.Marshal(proj)
	if err := os.MkdirAll(filepath.Join(dir, ".tokencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Load(Config{EventStop: {{Command: fmt.Sprintf("echo global >> %q", log)}}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	r.OnStop()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), "project\nglobal\n"; got != want {
		t.Fatalf("项目 hook 应先于全局执行：got %q want %q", got, want)
	}
}

func TestLoadErrors(t *testing.T) {
	// 坏正则。
	if _, err := Load(Config{EventPreToolUse: {{Matcher: "(", Command: "true"}}}, t.TempDir()); err == nil {
		t.Fatal("坏 matcher 正则应报错")
	}
	// 缺 command。
	if _, err := Load(Config{EventStop: {{Command: "  "}}}, t.TempDir()); err == nil {
		t.Fatal("空 command 应报错")
	}
	// 项目级文件是坏 JSON。
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".tokencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(nil, dir); err == nil {
		t.Fatal("项目级坏 JSON 应报错")
	}
}

func TestBlockStopsRemainingHooks(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "ran-second")
	r := load(t, Config{EventPreToolUse: {
		{Command: "exit 2"},
		{Command: fmt.Sprintf("touch %q", mark)},
	}})
	if block, _ := r.OnPreTool("bash", nil); !block {
		t.Fatal("第一条 hook 应阻断")
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("阻断后不该继续执行余下 hooks")
	}
}
