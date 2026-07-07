package pulse

import (
	"fmt"
	"strings"
	"testing"
)

func TestParsePlaybook(t *testing.T) {
	content := `# 长期记忆 playbook
（做梦机制增量维护；最近更新的在前）

- [build-cmd] go build -o bin/tokencode ./cmd/tokencode
- [user-lang] 中文注释与 commit message
- [build-cmd] 重复 id 应被忽略
一行旧格式笔记
`
	entries, legacy := parsePlaybook(content)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].ID != "build-cmd" || !strings.Contains(entries[0].Text, "go build") {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if legacy != "一行旧格式笔记" {
		t.Errorf("legacy = %q", legacy)
	}
}

func TestParseOpsToleratesNoise(t *testing.T) {
	out := "```\n好的，以下是操作：\nADD build-cmd | go test ./...\n- UPDATE user-lang | 中文优先\nDELETE stale-note\nNOOP\n这里是解释文字\nADD bad id here | 内容\nADD no-pipe 缺分隔符\n```"
	ops := parseOps(out)
	if len(ops) != 3 {
		t.Fatalf("ops = %+v", ops)
	}
	if ops[0].Kind != "add" || ops[0].ID != "build-cmd" {
		t.Errorf("ops[0] = %+v", ops[0])
	}
	if ops[1].Kind != "update" || ops[1].ID != "user-lang" || ops[1].Text != "中文优先" {
		t.Errorf("ops[1] = %+v", ops[1])
	}
	if ops[2].Kind != "delete" || ops[2].ID != "stale-note" {
		t.Errorf("ops[2] = %+v", ops[2])
	}
}

func TestParseOpsCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "ADD e%d | 条目 %d\n", i, i)
	}
	if got := len(parseOps(b.String())); got != maxOpsPerDream {
		t.Errorf("ops capped at %d, got %d", maxOpsPerDream, got)
	}
}

func TestApplyOps(t *testing.T) {
	entries := []playEntry{{ID: "a", Text: "旧 a"}, {ID: "b", Text: "b"}, {ID: "c", Text: "c"}}
	entries = applyOps(entries, []playOp{
		{Kind: "update", ID: "a", Text: "新 a"},
		{Kind: "delete", ID: "c"},
		{Kind: "add", ID: "d", Text: "d"},
	})
	// 最近更新的在前：d, a, b；c 已删。
	if len(entries) != 3 || entries[0].ID != "d" || entries[1].ID != "a" || entries[1].Text != "新 a" || entries[2].ID != "b" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestApplyOpsEviction(t *testing.T) {
	var entries []playEntry
	for i := 0; i < maxPlaybookEntries; i++ {
		entries = append(entries, playEntry{ID: fmt.Sprintf("e%d", i), Text: "x"})
	}
	entries = applyOps(entries, []playOp{{Kind: "add", ID: "fresh", Text: "新条目"}})
	if len(entries) != maxPlaybookEntries {
		t.Fatalf("len = %d", len(entries))
	}
	if entries[0].ID != "fresh" {
		t.Errorf("newest first, got %+v", entries[0])
	}
	// 被挤掉的是尾部（最久未更新）。
	last := entries[len(entries)-1]
	if last.ID == fmt.Sprintf("e%d", maxPlaybookEntries-1) {
		t.Error("tail should have been evicted")
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	in := []playEntry{{ID: "one", Text: "第一条"}, {ID: "two", Text: "第二条"}}
	out, legacy := parsePlaybook(renderPlaybook(in))
	if legacy != "" {
		t.Errorf("render 骨架不应产生 legacy：%q", legacy)
	}
	if len(out) != 2 || out[0] != in[0] || out[1] != in[1] {
		t.Errorf("round trip = %+v", out)
	}
}
