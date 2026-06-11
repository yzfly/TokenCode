package permrules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// compile 是测试便捷构造：坏规则直接 fail。
func compile(t *testing.T, l Lists) *Rules {
	t.Helper()
	r, warns := Compile(l)
	if len(warns) != 0 {
		t.Fatalf("unexpected warns: %v", warns)
	}
	return r
}

func bashInput(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func pathInput(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"path": p})
	return b
}

func TestParseRule(t *testing.T) {
	cases := []struct {
		in      string
		tool    string
		pattern string
		hasPat  bool
		wantErr bool
	}{
		{in: "read", tool: "read"},
		{in: "  WebSearch  ", tool: "websearch"}, // 工具名忽略大小写、剔空白
		{in: "bash(git log *)", tool: "bash", pattern: "git log *", hasPat: true},
		{in: "agent(explore)", tool: "agent", pattern: "explore", hasPat: true},
		{in: "mcp__github__*", tool: "mcp__github__*"},
		{in: "", wantErr: true},
		{in: "(x)", wantErr: true},
		{in: "bash(git log", wantErr: true}, // 括号没闭合
	}
	for _, c := range cases {
		r, err := ParseRule(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRule(%q): want error, got %+v", c.in, r)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRule(%q): %v", c.in, err)
			continue
		}
		if r.Tool != c.tool || r.Pattern != c.pattern || r.HasPat != c.hasPat {
			t.Errorf("ParseRule(%q) = %+v, want tool=%q pattern=%q hasPat=%v", c.in, r, c.tool, c.pattern, c.hasPat)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"git log *", "git log -5", true},
		{"git log *", "git log", false}, // 裸 glob 不含零参数语义（那在 patternMatch 层）
		{"git log *", "git logs", false},
		{"go test*", "go test ./...", true},
		{"go test*", "go test", true},
		{"*", "", true},
		{"*", "anything", true},
		{"a*c*e", "abcde", true},
		{"a*c*e", "ace", true},
		{"a*c*e", "abde", false},
		{"git log *", "GIT LOG -5", false}, // 参数模式大小写敏感
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git log", []string{"git log"}},
		{"a && b", []string{"a", "b"}},
		{"a || b", []string{"a", "b"}},
		{"a; b ;c", []string{"a", "b", "c"}},
		{"a | b", []string{"a", "b"}},
		{"a &", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{`echo "a && b"`, []string{`echo "a && b"`}},         // 双引号内不切
		{`echo 'x; y' && ls`, []string{`echo 'x; y'`, "ls"}}, // 单引号内不切
		{`echo a\;b`, []string{`echo a\;b`}},                 // 转义分号不切
		{"", nil},
		{"  ;  ", nil},
	}
	for _, c := range cases {
		got := SplitCommand(c.in)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("SplitCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEvaluateBash(t *testing.T) {
	r := compile(t, Lists{
		Allow: []string{"bash(git log *)", "bash(git status)", "bash(go test*)", "bash(ls *)"},
		Ask:   []string{"bash(git push *)"},
		Deny:  []string{"bash(rm -rf *)", "bash(git reset --hard*)"},
	})
	cases := []struct {
		cmd  string
		want Decision
	}{
		// 单段 allow：尾部 " *" 允许零参数
		{"git log -5", Allow},
		{"git log", Allow},
		{"git status", Allow},
		{"git logs", NoMatch},
		{"go test ./...", Allow},
		// deny 优先
		{"rm -rf /tmp/x", Deny},
		{"git reset --hard HEAD~1", Deny},
		// ask 介于两者之间（即使同时能命中 allow 也问）
		{"git push origin main", Ask},
		// 复合命令：全段 allow 才 allow
		{"git log && git status", Allow},
		{"git log; go test ./...", Allow},
		{"git log | head", NoMatch},         // head 没命中 allow
		{"git log && rm -rf /", Deny},       // 任一段 deny 即 deny
		{"git log && git push origin", Ask}, // 任一段 ask（无 deny）即 ask
		// 命令替换借壳：不许命中 allow
		{"git log $(rm -rf /)", NoMatch},
		{"ls `whoami`", NoMatch},
		{"", NoMatch},
	}
	for _, c := range cases {
		if got := r.Evaluate("bash", bashInput(c.cmd)); got != c.want {
			t.Errorf("Evaluate(bash, %q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestEvaluatePriority(t *testing.T) {
	// 同一调用同时命中三表：deny > ask > allow。
	r := compile(t, Lists{
		Allow: []string{"write"},
		Ask:   []string{"write"},
		Deny:  []string{"write"},
	})
	if got := r.Evaluate("write", pathInput("a.txt")); got != Deny {
		t.Fatalf("deny should win, got %v", got)
	}
	r = compile(t, Lists{Allow: []string{"write"}, Ask: []string{"write"}})
	if got := r.Evaluate("write", pathInput("a.txt")); got != Ask {
		t.Fatalf("ask should beat allow, got %v", got)
	}
}

func TestEvaluatePathRules(t *testing.T) {
	r := compile(t, Lists{
		Allow: []string{"read", "write(docs/*)"},
		Deny:  []string{"read(*secrets*)", "write(*.env)"},
	})
	cases := []struct {
		tool, path string
		want       Decision
	}{
		{"read", "main.go", Allow},            // 裸工具名规则
		{"read", "config/secrets.yaml", Deny}, // deny 压过裸 allow
		{"write", "docs/guide.md", Allow},
		{"write", "docs/sub/deep.md", Allow}, // * 跨路径分隔符（含 ** 语义）
		{"write", ".env", Deny},
		{"write", "src/main.go", NoMatch},
		{"edit", "a.go", NoMatch}, // edit 没有任何规则
	}
	for _, c := range cases {
		if got := r.Evaluate(c.tool, pathInput(c.path)); got != c.want {
			t.Errorf("Evaluate(%s, %q) = %v, want %v", c.tool, c.path, got, c.want)
		}
	}
}

func TestEvaluateAgentAndMCP(t *testing.T) {
	r := compile(t, Lists{
		Allow: []string{"agent(explore)", "mcp__github__*", "websearch"},
		Deny:  []string{"mcp__github__delete_*", "agent(racer*)"},
	})
	agentInput := func(typ string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"subagent_type": typ, "prompt": "x"})
		return b
	}
	cases := []struct {
		tool  string
		input json.RawMessage
		want  Decision
	}{
		{"agent", agentInput("explore"), Allow},
		{"agent", agentInput("racer#1"), Deny},
		{"agent", agentInput("review"), NoMatch},
		{"mcp__github__create_issue", nil, Allow},
		{"mcp__github__delete_repo", nil, Deny}, // deny 压过通配 allow
		{"mcp__jira__search", nil, NoMatch},
		{"websearch", nil, Allow},
		{"webfetch", nil, NoMatch},
	}
	for _, c := range cases {
		if got := r.Evaluate(c.tool, c.input); got != c.want {
			t.Errorf("Evaluate(%s) = %v, want %v", c.tool, got, c.want)
		}
	}
	// 没有参数模式语义的工具：带模式的规则一律不命中。
	r2 := compile(t, Lists{Allow: []string{"websearch(golang*)"}})
	if got := r2.Evaluate("websearch", json.RawMessage(`{"query":"golang glob"}`)); got != NoMatch {
		t.Fatalf("patterned rule on websearch should not match, got %v", got)
	}
}

func TestEvaluateToolNameCaseInsensitive(t *testing.T) {
	r := compile(t, Lists{Deny: []string{"Bash(rm *)"}})
	if got := r.Evaluate("bash", bashInput("rm -rf /")); got != Deny {
		t.Fatalf("tool name should match case-insensitively, got %v", got)
	}
}

func TestCompileWarns(t *testing.T) {
	r, warns := Compile(Lists{Allow: []string{"read", "(bad"}, Deny: []string{"bash(rm *)"}})
	if len(warns) != 1 {
		t.Fatalf("want 1 warn, got %v", warns)
	}
	// 坏规则跳过，好规则仍生效。
	if got := r.Evaluate("read", pathInput("a")); got != Allow {
		t.Fatalf("good rules should survive bad ones, got %v", got)
	}
	if got := r.Evaluate("bash", bashInput("rm -rf x")); got != Deny {
		t.Fatalf("deny should survive, got %v", got)
	}
}

func TestEmptyAndNil(t *testing.T) {
	var nilRules *Rules
	if !nilRules.Empty() || nilRules.Evaluate("bash", bashInput("rm -rf /")) != NoMatch {
		t.Fatal("nil rules should be empty and always NoMatch")
	}
	r, _ := Compile(Lists{})
	if !r.Empty() {
		t.Fatal("zero lists should compile to empty rules")
	}
}

func TestMergeAndLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".tokencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	proj := `{"allow": ["bash(go build*)"], "deny": ["bash(git push *)"]}`
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte(proj), 0o644); err != nil {
		t.Fatal(err)
	}
	global := Lists{Allow: []string{"read"}, Deny: []string{"bash(rm -rf *)"}}
	r, warns := Load(dir, global)
	if len(warns) != 0 {
		t.Fatalf("unexpected warns: %v", warns)
	}
	// 全局与项目级规则都生效。
	if got := r.Evaluate("read", pathInput("a")); got != Allow {
		t.Fatalf("global allow lost, got %v", got)
	}
	if got := r.Evaluate("bash", bashInput("go build ./...")); got != Allow {
		t.Fatalf("project allow lost, got %v", got)
	}
	if got := r.Evaluate("bash", bashInput("rm -rf /")); got != Deny {
		t.Fatalf("global deny lost, got %v", got)
	}
	if got := r.Evaluate("bash", bashInput("git push origin")); got != Deny {
		t.Fatalf("project deny lost, got %v", got)
	}

	// 项目文件损坏：降级只用全局并出警告。
	if err := os.WriteFile(filepath.Join(dir, ProjectFile), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, warns = Load(dir, global)
	if len(warns) != 1 {
		t.Fatalf("want 1 warn for broken project file, got %v", warns)
	}
	if got := r.Evaluate("bash", bashInput("rm -rf /")); got != Deny {
		t.Fatalf("global rules should survive broken project file, got %v", got)
	}

	// 项目文件不存在：只用全局，无警告。
	r, warns = Load(t.TempDir(), global)
	if len(warns) != 0 || r.Evaluate("read", pathInput("a")) != Allow {
		t.Fatalf("missing project file should be fine, warns=%v", warns)
	}
}
