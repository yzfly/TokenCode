package tui

import (
	"testing"

	"github.com/yzfly/tokencode/internal/skill"
)

// testModel 造一个带技能的最小 model（只测注册表纯逻辑，不起 Bubble Tea）。
func testModel() model {
	return model{
		perms:  newPerms(modeReview),
		skills: []skill.Skill{{Name: "deploy", Description: "部署"}},
	}
}

// TestLookupCommand 覆盖按名与按别名命中、未命中。
func TestLookupCommand(t *testing.T) {
	cmds := testModel().commands()

	if c, ok := lookupCommand(cmds, "help"); !ok || c.name != "help" {
		t.Fatalf("help not found: %+v", c)
	}
	if c, ok := lookupCommand(cmds, "quit"); !ok || c.name != "exit" {
		t.Fatalf("alias quit should hit exit: %+v ok=%v", c, ok)
	}
	if c, ok := lookupCommand(cmds, "deploy"); !ok || c.source != "skill" {
		t.Fatalf("skill command not registered: %+v ok=%v", c, ok)
	}
	if _, ok := lookupCommand(cmds, "nope"); ok {
		t.Fatal("unexpected hit for unknown command")
	}
}

// TestSuggestCommand 覆盖前缀建议与编辑距离建议（REQ-5：/exi → /exit）。
func TestSuggestCommand(t *testing.T) {
	cmds := testModel().commands()
	cases := map[string]string{
		"exi":    "exit",   // 前缀
		"hep":    "help",   // 距离 1
		"reveiw": "review", // 换位，距离 2
		"zzzzzz": "",       // 距离太远，不建议
	}
	for in, want := range cases {
		if got := suggestCommand(cmds, in); got != want {
			t.Errorf("suggest(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFilterCommands 覆盖补全过滤：前缀优先、子串次之、空前缀全量。
func TestFilterCommands(t *testing.T) {
	cmds := testModel().commands()

	all := filterCommands(cmds, "")
	if len(all) != len(cmds) {
		t.Fatalf("empty prefix should return all, got %d/%d", len(all), len(cmds))
	}

	got := filterCommands(cmds, "m")
	if len(got) == 0 || got[0].name != "model" || got[1].name != "mcp" {
		t.Fatalf("prefix matches should come first: %+v", names(got))
	}
	// "el" 是 help/model 的子串，无前缀命中。
	got = filterCommands(cmds, "el")
	if len(got) != 2 {
		t.Fatalf("substring matches wrong: %+v", names(got))
	}
	if len(filterCommands(cmds, "zzz")) != 0 {
		t.Fatal("expected no matches")
	}
}

func names(cmds []command) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.name
	}
	return out
}

// TestEditDistance 基本性质。
func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 3}, {"abc", "abc", 0}, {"exi", "exit", 1}, {"hep", "help", 1}, {"cat", "dog", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("dist(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
