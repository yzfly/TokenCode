package race

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// initRepo 建一个带初始提交的临时 git 仓库。
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	mustGit := func(args ...string) {
		t.Helper()
		if _, err := git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustGit("init", "-q")
	mustGit("config", "user.name", "test")
	mustGit("config", "user.email", "test@test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit("add", "-A")
	mustGit("commit", "-q", "-m", "init")
	// macOS 上 TempDir 带符号链接（/var → /private/var），统一解析。
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return dir
}

func TestWorktreeLifecycle(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	w, err := addWorktree(ctx, repo, t.TempDir(), "test1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 新文件 + 修改各一，diff 都要看得见。
	if err := os.WriteFile(filepath.Join(w.Dir, "new.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Dir, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := w.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "new.go") || !strings.Contains(diff, "changed") {
		t.Errorf("diff missing changes:\n%s", diff)
	}
	if st := w.DiffStat(ctx); !strings.Contains(st, "2 files") {
		t.Errorf("diffstat = %q", st)
	}

	// 主工作区不受影响。
	if b, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(b) != "hello\n" {
		t.Error("main worktree was polluted")
	}

	// 删除（不留分支）后 worktree 目录与分支都没了。
	if err := w.Remove(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Dir); !os.IsNotExist(err) {
		t.Error("worktree dir should be gone")
	}
	if _, err := git(ctx, repo, "rev-parse", "--verify", w.Branch); err == nil {
		t.Error("branch should be deleted")
	}
}

// fakeComplete 是裁判模型替身：按 diff 里埋的 quality-N 标记打分，
// 终审选标记最大的实现。
func fakeComplete(ctx context.Context, system, user string) (string, error) {
	re := regexp.MustCompile(`quality-(\d+)`)
	if strings.Contains(system, "终审") {
		// 逐段找编号与质量标记，选标记最大的。
		best, bestQ := 0, -1
		secs := regexp.MustCompile(`===== 实现 (\d+)（[^）]*）=====`).FindAllStringSubmatchIndex(user, -1)
		for k, m := range secs {
			end := len(user)
			if k+1 < len(secs) {
				end = secs[k+1][0]
			}
			seg := user[m[1]:end]
			var idx int
			fmt.Sscanf(user[m[2]:m[3]], "%d", &idx)
			if qm := re.FindStringSubmatch(seg); qm != nil {
				var q int
				fmt.Sscanf(qm[1], "%d", &q)
				if q > bestQ {
					best, bestQ = idx, q
				}
			}
		}
		return fmt.Sprintf(`选 %d。{"winner": %d, "reason": "标记最高"}`, best, best), nil
	}
	// 打分：quality-N → N 分。
	if m := re.FindStringSubmatch(user); m != nil {
		return fmt.Sprintf(`{"score": %s, "reason": "按标记"}`, m[1]), nil
	}
	return `{"score": 1, "reason": "无标记"}`, nil
}

func TestRunRace(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// 6 个 racer：#0 什么都不写（空 diff 出局），其余写出质量递增的方案，
	// 幸存 5 个 > finalists，会走完整的打分→决赛路径。
	spawn := func(ctx context.Context, i int, prompt, dir string) (string, error) {
		if i == 0 {
			return "did nothing", nil
		}
		content := fmt.Sprintf("solution quality-%d\n", i)
		if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte(content), 0o644); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote answer.txt with quality-%d", i), nil
	}

	var phases []string
	res, err := Run(ctx, Options{
		N:           6,
		Task:        "write the best answer.txt",
		Concurrency: 3,
		RepoRoot:    repo,
	}, Deps{
		Spawn:    spawn,
		Complete: fakeComplete,
		Progress: func(p Progress) { phases = append(phases, p.Phase) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner == nil {
		t.Fatal("no winner")
	}
	if res.Winner.Index != 5 {
		t.Errorf("winner = #%d, want #5 (highest quality)", res.Winner.Index)
	}
	if !strings.Contains(res.Winner.Diff, "quality-5") {
		t.Error("winner diff should carry its content")
	}
	// 排行榜：6 行，#0 出局殿后。
	if len(res.Board) != 6 {
		t.Fatalf("board size = %d", len(res.Board))
	}
	last := res.Board[len(res.Board)-1]
	if last.Index != 0 || last.Out == "" {
		t.Errorf("eliminated racer should be last on board, got #%d (%q)", last.Index, last.Out)
	}

	// 冠军分支保留、败者分支清光、worktree 全部移除。
	if _, err := git(ctx, repo, "rev-parse", "--verify", res.Winner.Branch); err != nil {
		t.Errorf("winner branch should be kept: %v", err)
	}
	out, _ := git(ctx, repo, "branch", "--list", "tokencode/race-*")
	if n := len(strings.Fields(out)); n != 1 {
		t.Errorf("loser branches should be deleted, got: %q", out)
	}
	wt, _ := git(ctx, repo, "worktree", "list")
	if strings.Count(strings.TrimSpace(wt), "\n") != 0 {
		t.Errorf("all worktrees should be removed:\n%s", wt)
	}

	// 进度经过三个阶段。
	joined := strings.Join(phases, ",")
	for _, ph := range []string{"racing", "judging", "final"} {
		if !strings.Contains(joined, ph) {
			t.Errorf("progress missing phase %s", ph)
		}
	}

	// 冠军 diff 能应用回主工作区。
	if err := Apply(ctx, repo, res.Winner.Diff); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(repo, "answer.txt"))
	if err != nil || !strings.Contains(string(b), "quality-5") {
		t.Errorf("applied content = %q, %v", b, err)
	}
}

func TestRunRaceGoodEnough(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// 全员都能写出 quality-9（≥ 够好线 5）：窗口压成 1，第一个冲线者
	// 当场夺冠，其余 5 个直接退钱（排队的不再起跑）。
	spawn := func(ctx context.Context, i int, prompt, dir string) (string, error) {
		content := fmt.Sprintf("solution by %d quality-9\n", i)
		return "done quality-9", os.WriteFile(filepath.Join(dir, "answer.txt"), []byte(content), 0o644)
	}
	var last Progress
	res, err := Run(ctx, Options{
		N: 6, Task: "t", Concurrency: 1, RepoRoot: repo, GoodEnough: 5,
	}, Deps{Spawn: spawn, Complete: fakeComplete, Progress: func(p Progress) { last = p }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner == nil {
		t.Fatal("no winner")
	}
	if res.Winner.Score != 9 {
		t.Errorf("winner score = %d, want 9", res.Winner.Score)
	}
	if !strings.Contains(res.Reason, "够好即收") {
		t.Errorf("reason = %q, want 够好即收", res.Reason)
	}

	// 败者 5 个全部标注退钱，进度也计入 Refunded。
	refunded := 0
	for _, c := range res.Board {
		if c.Out == OutRefund {
			refunded++
		}
	}
	if refunded != 5 {
		t.Errorf("refunded = %d, want 5\nboard: %+v", refunded, res.Board)
	}
	if last.Refunded != 5 {
		t.Errorf("progress.Refunded = %d, want 5", last.Refunded)
	}

	// 冠军分支保留，其余分支与 worktree 全清。
	if _, err := git(ctx, repo, "rev-parse", "--verify", res.Winner.Branch); err != nil {
		t.Errorf("winner branch should be kept: %v", err)
	}
	out, _ := git(ctx, repo, "branch", "--list", "tokencode/race-*")
	if n := len(strings.Fields(out)); n != 1 {
		t.Errorf("loser branches should be deleted, got: %q", out)
	}
	wt, _ := git(ctx, repo, "worktree", "list")
	if strings.Count(strings.TrimSpace(wt), "\n") != 0 {
		t.Errorf("all worktrees should be removed:\n%s", wt)
	}
}

func TestRunRaceGoodEnoughNoTrigger(t *testing.T) {
	repo := initRepo(t)
	// 无人达到够好线：退化为全员跑完，初评已在赛中完成，直接终审择优。
	spawn := func(ctx context.Context, i int, prompt, dir string) (string, error) {
		content := fmt.Sprintf("solution quality-%d\n", i+1)
		return "done", os.WriteFile(filepath.Join(dir, "answer.txt"), []byte(content), 0o644)
	}
	res, err := Run(context.Background(), Options{
		N: 3, Task: "t", RepoRoot: repo, GoodEnough: 9,
	}, Deps{Spawn: spawn, Complete: fakeComplete})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner == nil || res.Winner.Index != 2 {
		t.Fatalf("winner = %+v, want #2 (highest quality)", res.Winner)
	}
	for _, c := range res.Board {
		if c.Out != "" {
			t.Errorf("#%d should survive, got out: %q", c.Index, c.Out)
		}
		if c.Score != c.Index+1 {
			t.Errorf("#%d score = %d, want %d (scored during race)", c.Index, c.Score, c.Index+1)
		}
	}
}

func TestRunRaceCheckEliminates(t *testing.T) {
	repo := initRepo(t)
	// 两个 racer 都写文件，但客观校验只放过 #1。
	spawn := func(ctx context.Context, i int, prompt, dir string) (string, error) {
		name := "bad.txt"
		if i == 1 {
			name = "good.txt"
		}
		return "done", os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644)
	}
	res, err := Run(context.Background(), Options{
		N: 2, Task: "t", RepoRoot: repo, Check: "test -f good.txt",
	}, Deps{Spawn: spawn, Complete: fakeComplete})
	if err != nil {
		t.Fatal(err)
	}
	if res.Winner == nil || res.Winner.Index != 1 {
		t.Fatalf("winner = %+v, want #1", res.Winner)
	}
	if res.Board[len(res.Board)-1].Out == "" {
		t.Error("check failure should eliminate")
	}
}

func TestRunRaceAllFail(t *testing.T) {
	repo := initRepo(t)
	spawn := func(ctx context.Context, i int, prompt, dir string) (string, error) {
		return "nothing", nil // 全员空 diff
	}
	res, err := Run(context.Background(), Options{N: 2, Task: "t", RepoRoot: repo},
		Deps{Spawn: spawn, Complete: fakeComplete})
	if err == nil {
		t.Fatal("expected error on total wipeout")
	}
	if res == nil || len(res.Board) != 2 {
		t.Fatal("board should still report failures")
	}
	// 不留任何分支与 worktree。
	out, _ := git(context.Background(), repo, "branch", "--list", "tokencode/race-*")
	if strings.TrimSpace(out) != "" {
		t.Errorf("no branches should remain: %q", out)
	}
}

func TestRunRaceBadN(t *testing.T) {
	for _, n := range []int{0, MaxN + 1} {
		if _, err := Run(context.Background(), Options{N: n, Task: "t", RepoRoot: "."},
			Deps{Spawn: func(context.Context, int, string, string) (string, error) { return "", nil },
				Complete: fakeComplete}); err == nil {
			t.Errorf("N=%d should be rejected", n)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	got := string(extractJSON("好的，我的评分是：\n```json\n{\"score\": 7, \"reason\": \"ok\"}\n```"))
	if got != `{"score": 7, "reason": "ok"}` {
		t.Errorf("extractJSON = %q", got)
	}
}
