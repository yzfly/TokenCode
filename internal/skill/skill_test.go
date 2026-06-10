package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSkill 在 root/skills/<dir>/SKILL.md 写一个技能。
func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscover 验证多级目录发现、frontmatter 解析、同名去重（项目级压过用户级）、
// 兼容 .claude/skills。
func TestDiscover(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeSkill(t, filepath.Join(cwd, ".tokencode", "skills"), "deploy",
		"---\nname: deploy\ndescription: 部署到生产\n---\n按步骤部署。\n")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "review",
		"---\ndescription: \"代码评审\"\n---\n评审这些变更。\n")
	// 用户级同名 deploy，应被项目级压过。
	writeSkill(t, filepath.Join(home, ".config", "tokencode", "skills"), "deploy",
		"---\nname: deploy\ndescription: 旧版部署\n---\n旧正文。\n")
	// 无 frontmatter：名字取目录名。
	writeSkill(t, filepath.Join(home, ".config", "tokencode", "skills"), "bare", "直接正文。\n")

	got := Discover(cwd)
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 skills, got %d: %+v", len(got), got)
	}
	if byName["deploy"].Description != "部署到生产" || byName["deploy"].Source != "project" {
		t.Fatalf("deploy wrong: %+v", byName["deploy"])
	}
	if byName["review"].Description != "代码评审" || byName["review"].Source != "project(claude)" {
		t.Fatalf("review wrong: %+v", byName["review"])
	}
	if byName["bare"].Description != "" || byName["bare"].Source != "user" {
		t.Fatalf("bare wrong: %+v", byName["bare"])
	}
}

// TestExpand 验证正文懒加载、$ARGUMENTS 替换与无占位符时的追加语义。
func TestExpand(t *testing.T) {
	cwd := t.TempDir()
	writeSkill(t, filepath.Join(cwd, ".tokencode", "skills"), "with-args",
		"---\nname: with-args\n---\n处理 $ARGUMENTS 然后报告。\n")
	writeSkill(t, filepath.Join(cwd, ".tokencode", "skills"), "no-args",
		"---\nname: no-args\n---\n固定流程。\n")

	skills := Discover(cwd)
	byName := map[string]Skill{}
	for _, s := range skills {
		byName[s.Name] = s
	}

	out, err := byName["with-args"].Expand("issue-42")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out != "处理 issue-42 然后报告。\n" {
		t.Fatalf("substitution wrong: %q", out)
	}

	out, err = byName["no-args"].Expand("extra context")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out != "固定流程。\n\nARGUMENTS: extra context" {
		t.Fatalf("append wrong: %q", out)
	}

	out, err = byName["no-args"].Expand("")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if out != "固定流程。\n" {
		t.Fatalf("no-args plain wrong: %q", out)
	}
}
