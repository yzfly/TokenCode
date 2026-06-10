// Package race 实现并行竞赛模式（ROADMAP 阶段 2 · A · 横向爆破）：
// 同一个任务派 N 个 agent（N≤1000）各自在隔离的 git worktree 里独立解，
// 跑完后裁判流水线（客观粗筛 → 并行打分 → 决赛）选出冠军，
// 经用户确认把冠军 diff 应用回主工作区。
//
// 包本身零内部依赖：racer 怎么跑（Spawn）、裁判模型怎么调（Complete）
// 都由调用方注入，这里只负责编排与 git 资源生命周期。
package race

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
)

// MaxN 是单场竞赛的 racer 总数上限。
const MaxN = 1000

// defaultConcurrency 是同时在飞的 racer 默认窗口（LLM 限流与本机资源的折中）。
const defaultConcurrency = 8

// SpawnFunc 跑一个 racer：在 dir（它的 worktree）里完成 task，返回实现报告。
type SpawnFunc func(ctx context.Context, index int, prompt, dir string) (report string, err error)

// CompleteFunc 是裁判的纯文本模型调用。
type CompleteFunc func(ctx context.Context, system, user string) (string, error)

// Options 是一场竞赛的参数。
type Options struct {
	N           int
	Task        string
	Concurrency int    // 同时在飞窗口；≤0 用默认
	Check       string // 客观校验命令（在各 worktree 内跑）；空=跳过
	RepoRoot    string // git 仓库根
	// Variant 生成第 i 个 racer 的任务提示（多样性扩展点）；nil=恒等。
	Variant func(index int, task string) string
}

// Deps 是注入的执行依赖。
type Deps struct {
	Spawn    SpawnFunc
	Complete CompleteFunc
	Progress func(Progress) // 进度回调，可为 nil
}

// Progress 是竞赛的聚合进度快照。
type Progress struct {
	Phase   string // "racing" | "judging" | "final"
	N       int
	Queued  int
	Running int
	Done    int
	Failed  int
	Scored  int // judging 阶段已打分数
	Judging int // judging 阶段的幸存者总数
}

// Candidate 是一个 racer 的最终产物。
type Candidate struct {
	Index    int
	Branch   string
	Report   string
	Diff     string
	DiffStat string
	Score    int
	Reason   string // 打分理由（或决赛理由）
	Out      string // 出局原因（spawn 失败/diff 为空/check 失败）；空=幸存进入裁判
}

// Result 是一场竞赛的结果。
type Result struct {
	RunID    string
	RepoRoot string
	Winner   *Candidate // 冠军（分支保留）；nil=全军覆没
	Reason   string     // 终审理由
	Board    []Candidate
}

// Run 跑完一场竞赛。ctx 取消（用户打断）传导到所有 racer 并清理全部 worktree。
func Run(ctx context.Context, o Options, deps Deps) (*Result, error) {
	if o.N < 1 || o.N > MaxN {
		return nil, fmt.Errorf("N 必须在 1-%d 之间（收到 %d）", MaxN, o.N)
	}
	if strings.TrimSpace(o.Task) == "" {
		return nil, fmt.Errorf("任务描述不能为空")
	}
	if deps.Spawn == nil || deps.Complete == nil {
		return nil, fmt.Errorf("race: Spawn 与 Complete 都必须注入")
	}
	repo, err := RepoRoot(ctx, o.RepoRoot)
	if err != nil {
		return nil, err
	}
	window := o.Concurrency
	if window <= 0 {
		window = defaultConcurrency
	}
	variant := o.Variant
	if variant == nil {
		variant = func(_ int, task string) string { return task }
	}

	runID := newRunID()
	baseDir, err := os.MkdirTemp("", "tokencode-race-"+runID+"-")
	if err != nil {
		return nil, fmt.Errorf("创建竞赛临时目录: %w", err)
	}
	defer os.RemoveAll(baseDir)

	// ---- 阶段 1：窗口化扇出。worktree 在 racer 进窗口时才创建（摊开成本），
	// 出局者当场清理，存活者保留到裁判结束。
	cands := make([]*Candidate, o.N)
	trees := make([]*worktree, o.N)
	var mu sync.Mutex
	prog := Progress{Phase: "racing", N: o.N, Queued: o.N}
	report := func(mut func(*Progress)) {
		mu.Lock()
		mut(&prog)
		snap := prog
		mu.Unlock()
		if deps.Progress != nil {
			deps.Progress(snap)
		}
	}

	sem := make(chan struct{}, window)
	var wg sync.WaitGroup
	for i := 0; i < o.N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &Candidate{Index: i}
			cands[i] = c
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				c.Out = "已取消"
				report(func(p *Progress) { p.Queued--; p.Failed++ })
				return
			}
			report(func(p *Progress) { p.Queued--; p.Running++ })
			ok := runRacer(ctx, c, &trees[i], o, deps, repo, baseDir, runID, variant)
			report(func(p *Progress) {
				p.Running--
				if ok {
					p.Done++
				} else {
					p.Failed++
				}
			})
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		cleanupAll(trees, repo)
		return nil, ctx.Err()
	}

	// ---- 阶段 2：裁判。幸存者 = 有非空 diff 且过了 check 的候选。
	var alive []*Candidate
	for _, c := range cands {
		if c.Out == "" {
			alive = append(alive, c)
		}
	}
	if len(alive) == 0 {
		cleanupAll(trees, repo)
		board := snapshot(cands)
		return &Result{RunID: runID, RepoRoot: repo, Board: board},
			fmt.Errorf("全军覆没：%d 个 racer 无一产出可用改动", o.N)
	}

	report(func(p *Progress) { p.Phase = "judging"; p.Judging = len(alive); p.Scored = 0 })
	if len(alive) > finalists {
		judgeScores(ctx, deps.Complete, o.Task, alive, window, func() {
			report(func(p *Progress) { p.Scored++ })
		})
	}
	sortByScore(alive)

	report(func(p *Progress) { p.Phase = "final" })
	winIdx, reason, err := judgeFinal(ctx, deps.Complete, o.Task, alive)
	if err != nil {
		// 终审失败退化为按初评分取最高（分数全 0 时即第一个幸存者）。
		winIdx, reason = 0, "终审失败，按初评分取最高: "+err.Error()
	}
	winner := alive[winIdx]
	winner.Reason = reason

	// ---- 阶段 3：清理。冠军留分支，其余 worktree+分支全删。
	for i, t := range trees {
		if t == nil {
			continue
		}
		_ = t.Remove(context.WithoutCancel(ctx), i == winner.Index)
	}

	return &Result{
		RunID:    runID,
		RepoRoot: repo,
		Winner:   winner,
		Reason:   reason,
		Board:    snapshot(cands),
	}, ctx.Err()
}

// runRacer 跑单个 racer 的完整生命周期：建 worktree → spawn → 收 diff → 粗筛。
// 返回是否幸存；出局者的 worktree 当场清理（tree 置 nil）。
func runRacer(ctx context.Context, c *Candidate, tree **worktree, o Options, deps Deps,
	repo, baseDir, runID string, variant func(int, string) string) bool {
	out := func(reason string) bool {
		c.Out = reason
		if *tree != nil {
			_ = (*tree).Remove(context.WithoutCancel(ctx), false)
			*tree = nil
		}
		return false
	}

	w, err := addWorktree(ctx, repo, baseDir, runID, c.Index)
	if err != nil {
		return out("建 worktree 失败: " + err.Error())
	}
	*tree = &w
	c.Branch = w.Branch

	report, err := deps.Spawn(ctx, c.Index, variant(c.Index, o.Task), w.Dir)
	if err != nil {
		return out("racer 失败: " + err.Error())
	}
	c.Report = report

	diff, err := w.Diff(ctx)
	if err != nil {
		return out("收集 diff 失败: " + err.Error())
	}
	if strings.TrimSpace(diff) == "" {
		return out("没有产出任何改动")
	}
	c.Diff = diff
	c.DiffStat = w.DiffStat(ctx)

	if o.Check != "" {
		if checkOut, err := w.RunCheck(ctx, o.Check); err != nil {
			return out("客观校验未通过: " + lastLines(checkOut, 5))
		}
	}
	return true
}

// cleanupAll 清掉所有还活着的 worktree（取消/全灭路径）。
func cleanupAll(trees []*worktree, repo string) {
	ctx := context.Background()
	for _, t := range trees {
		if t != nil {
			_ = t.Remove(ctx, false)
		}
	}
	_, _ = git(ctx, repo, "worktree", "prune")
}

func snapshot(cands []*Candidate) []Candidate {
	board := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c == nil {
			continue
		}
		cc := *c
		cc.Diff = "" // 排行榜不背着全部 diff（冠军的留在 Winner 里）
		board = append(board, cc)
	}
	// 幸存者按分数降序在前，出局者殿后。
	sortBoard(board)
	return board
}

func sortBoard(board []Candidate) {
	// 简单插入语义：幸存者(Out=="")在前按分数降序，出局者按 Index。
	alive, dead := board[:0:0], []Candidate{}
	for _, c := range board {
		if c.Out == "" {
			alive = append(alive, c)
		} else {
			dead = append(dead, c)
		}
	}
	ptrs := make([]*Candidate, len(alive))
	for i := range alive {
		ptrs[i] = &alive[i]
	}
	sortByScore(ptrs)
	out := make([]Candidate, 0, len(board))
	for _, p := range ptrs {
		out = append(out, *p)
	}
	out = append(out, dead...)
	copy(board, out)
}

// lastLines 取文本最后 n 行（校验失败时只回传结尾的报错）。
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func newRunID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

// RacerSystem 是 racer 的契约系统提示（调用方据此构造子代理定义）。
// racer 的工具已被锁定在自己的 worktree：相对路径即可，绝不要试图访问外部。
const RacerSystem = `You are one of several agents independently competing to solve the SAME task.
Work entirely inside your own working directory (your tools are confined to it).
Always use paths RELATIVE to your working directory.

Rules of the competition:
- Implement the task fully: make real changes on disk, don't just describe a plan.
- Verify your own work where possible (build, run tests) before finishing.
- A judge will compare your changes (git diff) and your final report against the
  other contestants'. Empty diffs are eliminated immediately.
- Your final message must be a concise implementation report: what you changed,
  which files, and how you verified it.`
