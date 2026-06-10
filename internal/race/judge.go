package race

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// 裁判输入的 diff 截断预算（字符）。打分是单份评估可以宽一些；
// 决赛要并排放最多 finalists 份，得紧一些。
const (
	scoreDiffBudget = 24000
	finalDiffBudget = 10000
	finalists       = 4
)

// judgeScores 并行给每个幸存者打分（写回 c.Score/c.Reason）。
// 单个裁判失败不致命：解析失败重试一次，再失败记 0 分留个说明。
func judgeScores(ctx context.Context, complete CompleteFunc, task string, cands []*Candidate, window int, onProgress func()) {
	if window < 1 {
		window = 1
	}
	sem := make(chan struct{}, window)
	var wg sync.WaitGroup
	for _, c := range cands {
		wg.Add(1)
		go func(c *Candidate) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			score, reason, err := scoreOne(ctx, complete, task, c)
			if err != nil {
				c.Score, c.Reason = 0, "裁判失败: "+err.Error()
			} else {
				c.Score, c.Reason = score, reason
			}
			if onProgress != nil {
				onProgress()
			}
		}(c)
	}
	wg.Wait()
}

func scoreOne(ctx context.Context, complete CompleteFunc, task string, c *Candidate) (int, string, error) {
	system := `你是代码竞赛的评分裁判。N 个 agent 独立解了同一个任务，你逐份评估。
评分维度：是否真正完成任务、改动的正确性、是否最小且聚焦、代码质量。
只输出一行 JSON，别的什么都不要：{"score": <1-10 整数>, "reason": "<一句话理由>"}`
	user := fmt.Sprintf("任务：\n%s\n\nagent 的实现报告：\n%s\n\nagent 的改动（git diff）：\n%s",
		task, truncate(c.Report, 4000), truncate(c.Diff, scoreDiffBudget))

	var lastErr error
	for try := 0; try < 2; try++ {
		out, err := complete(ctx, system, user)
		if err != nil {
			lastErr = err
			continue
		}
		var v struct {
			Score  int    `json:"score"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(extractJSON(out), &v); err != nil {
			lastErr = fmt.Errorf("评分输出无法解析: %.80s", out)
			continue
		}
		if v.Score < 1 {
			v.Score = 1
		}
		if v.Score > 10 {
			v.Score = 10
		}
		return v.Score, v.Reason, nil
	}
	return 0, "", lastErr
}

// judgeFinal 让终审裁判在 top 候选里横向对比选出冠军，返回冠军在 cands
// 里的下标与理由。cands 须已按分数降序排好；只取前 finalists 份。
func judgeFinal(ctx context.Context, complete CompleteFunc, task string, cands []*Candidate) (int, string, error) {
	n := len(cands)
	if n == 0 {
		return -1, "", fmt.Errorf("没有候选")
	}
	if n == 1 {
		return 0, "唯一幸存者", nil
	}
	if n > finalists {
		n = finalists
	}

	var b strings.Builder
	fmt.Fprintf(&b, "任务：\n%s\n\n以下是 %d 份独立实现，编号 0-%d：\n", task, n, n-1)
	for i := 0; i < n; i++ {
		c := cands[i]
		fmt.Fprintf(&b, "\n===== 实现 %d（初评 %d 分）=====\n报告：%s\n改动：\n%s\n",
			i, c.Score, truncate(c.Report, 2000), truncate(c.Diff, finalDiffBudget))
	}
	system := fmt.Sprintf(`你是代码竞赛的终审裁判，从 %d 份独立实现中选出最好的一份。
对比标准：任务完成度优先，其次正确性，再次改动是否最小且聚焦。
只输出一行 JSON：{"winner": <0-%d 整数>, "reason": "<一句话理由>"}`, n, n-1)

	var lastErr error
	for try := 0; try < 2; try++ {
		out, err := complete(ctx, system, b.String())
		if err != nil {
			lastErr = err
			continue
		}
		var v struct {
			Winner int    `json:"winner"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(extractJSON(out), &v); err != nil || v.Winner < 0 || v.Winner >= n {
			lastErr = fmt.Errorf("终审输出无法解析: %.80s", out)
			continue
		}
		return v.Winner, v.Reason, nil
	}
	return -1, "", lastErr
}

// sortByScore 把候选按分数降序排（同分按 index 稳定）。
func sortByScore(cands []*Candidate) {
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
}

// extractJSON 从模型输出里抠出第一段 {...}（容忍裁判话痨或代码围栏）。
func extractJSON(s string) []byte {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return []byte(s)
	}
	return []byte(s[start : end+1])
}

// truncate 把超预算的文本截断并标注（裁判看得到截断事实）。
func truncate(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	return s[:budget] + fmt.Sprintf("\n...[已截断，原文 %d 字符]", len(s))
}
