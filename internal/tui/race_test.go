package tui

import (
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/race"
)

func TestRacePanelText(t *testing.T) {
	cases := []struct {
		p    race.Progress
		want string
	}{
		{race.Progress{Phase: "racing", N: 24, Running: 8, Queued: 12, Done: 3, Failed: 1}, "8 运行"},
		{race.Progress{Phase: "judging", Scored: 5, Judging: 7}, "5/7"},
		{race.Progress{Phase: "final"}, "终审"},
	}
	for _, c := range cases {
		if got := racePanelText(c.p); !strings.Contains(got, c.want) {
			t.Errorf("racePanelText(%s) = %q, want contains %q", c.p.Phase, got, c.want)
		}
	}
}

func TestRaceBoardText(t *testing.T) {
	res := &race.Result{
		RepoRoot: "/repo",
		Reason:   "完成度最高",
		Winner:   &race.Candidate{Index: 2, Branch: "tokencode/race-ab-2", Reason: "完成度最高", DiffStat: "3 files changed"},
		Board: []race.Candidate{
			{Index: 2, Score: 9, Reason: "完成度最高"},
			{Index: 1, Score: 6, Reason: "可用但粗糙"},
			{Index: 0, Out: "没有产出任何改动"},
		},
	}
	got := raceBoardText(res)
	for _, want := range []string{"冠军 #2", "tokencode/race-ab-2", "3 files changed", "出局", "/race apply"} {
		if !strings.Contains(got, want) {
			t.Errorf("board missing %q in:\n%s", want, got)
		}
	}

	// 无冠军（全灭）也能渲染。
	empty := raceBoardText(&race.Result{Board: []race.Candidate{{Index: 0, Out: "x"}}})
	if !strings.Contains(empty, "无冠军") || strings.Contains(empty, "apply") {
		t.Errorf("wipeout board wrong:\n%s", empty)
	}
}
