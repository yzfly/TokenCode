package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOneLineCollapsesAndTruncatesUTF8(t *testing.T) {
	// 多行压成一行。
	if got := oneLine("a\nb\nc", 100); got != "a b c" {
		t.Fatalf("collapse: got %q", got)
	}
	// 制表符与连续空格也压成单空格（read 的 cat -n 结果就是这种）。
	if got := oneLine("   1\tpackage tui\n   2\t\n   3\timport (", 100); got != "1 package tui 2 3 import (" {
		t.Fatalf("collapse tabs: got %q", got)
	}
	// 按 rune 截断（不能把中文截半）。
	in := strings.Repeat("中", 10)
	got := oneLine(in, 3)
	if got != "中中中…" {
		t.Fatalf("truncate: got %q", got)
	}
	// 截断点必须是合法 UTF-8。
	if !json.Valid([]byte(`"` + got + `"`)) {
		t.Fatalf("truncated string is not valid UTF-8: %q", got)
	}
}

func TestCompactJSON(t *testing.T) {
	raw := json.RawMessage("{\n  \"path\": \"a.txt\",\n  \"content\": \"hi\"\n}")
	got := compactJSON(raw)
	if got != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("compactJSON: got %q", got)
	}
	// 非法 JSON 原样返回，不 panic。
	if got := compactJSON(json.RawMessage("not json")); got != "not json" {
		t.Fatalf("invalid passthrough: got %q", got)
	}
}

func TestInterpretConfirmKey(t *testing.T) {
	cases := map[byte]confirmChoice{
		'y': choiceAllowOnce,
		'Y': choiceAllowOnce,
		'a': choiceAllowAlways,
		'A': choiceAllowAlways,
		'n': choiceReject,
		'N': choiceReject,
		13:  choiceReject, // Enter
		27:  choiceReject, // Esc
		3:   choiceReject, // Ctrl-C
		' ': choiceReject,
	}
	for b, want := range cases {
		if got := interpretConfirmKey(b); got != want {
			t.Fatalf("key %d: got %v want %v", b, got, want)
		}
	}
}
