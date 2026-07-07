package subagent

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Job 状态：running（在跑）→ done（有结果）/ failed（出错）。
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Job 是一个后台子代理句柄：spawn_agent 立即返回 id，代理在后台跑；
// wait_agent 取结果；resume_agent 在保留的上下文上续聊。
type Job struct {
	ID     string
	Type   string
	Status string
	Result string // done=最终文本 / failed=错误信息

	sub  *jobAgent // 保住实例与 UI：resume 续拍要用
	done chan struct{}
	busy bool // resume 互斥：同一句柄不允许并发跑拍
}

// jobAgent 打包一个装配好的子代理及其 UI 通道。
type jobAgent struct {
	run func(ctx context.Context, prompt string) (string, error)
}

// JobView 是 Job 的只读快照（list_agents 用）。
type JobView struct {
	ID, Type, Status, Result string
}

// SpawnAsync 后台启动一个子代理，立即返回句柄 id。装配错误（未知类型、
// 模型解析失败）同步返回；执行错误落在句柄状态里由 wait_agent 取。
// 后台拍不绑发起 turn 的 ctx——工具调用返回后代理继续跑，这正是异步的意义。
func (r *Runner) SpawnAsync(typ, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("子代理任务 prompt 不能为空")
	}
	def, ok := r.lookup(typ)
	if !ok {
		names := make([]string, 0, len(r.defs))
		for _, d := range r.defs {
			names = append(names, d.Name)
		}
		return "", fmt.Errorf("未知子代理类型 %q（可用：%s）", typ, strings.Join(names, ", "))
	}

	r.jobMu.Lock()
	r.jobSeq++
	id := fmt.Sprintf("ag-%d", r.jobSeq)
	r.jobMu.Unlock()

	sub, ui, label, err := r.assemble(def, SpawnOpts{Label: typ + ":" + id})
	if err != nil {
		return "", err
	}
	job := &Job{
		ID: id, Type: typ, Status: JobRunning,
		done: make(chan struct{}),
		sub: &jobAgent{run: func(ctx context.Context, prompt string) (string, error) {
			final, err := runOnce(ctx, sub, ui, prompt)
			if err != nil {
				return "", fmt.Errorf("子代理 %s: %w", label, err)
			}
			return final, nil
		}},
	}
	r.jobMu.Lock()
	if r.jobs == nil {
		r.jobs = map[string]*Job{}
	}
	r.jobs[id] = job
	r.jobMu.Unlock()

	go func() {
		// 并发防线与同步 spawn 共用同一个信号量。
		r.sem <- struct{}{}
		defer func() { <-r.sem }()
		final, err := job.sub.run(context.Background(), prompt)
		r.jobMu.Lock()
		if err != nil {
			job.Status, job.Result = JobFailed, err.Error()
		} else {
			job.Status, job.Result = JobDone, final
		}
		r.jobMu.Unlock()
		close(job.done)
	}()
	return id, nil
}

// WaitJob 阻塞等待句柄结束并返回结果；ctx 取消（用户打断当前拍）时返回错误，
// 后台代理继续跑，之后可再 wait。
func (r *Runner) WaitJob(ctx context.Context, id string) (string, error) {
	job, err := r.job(id)
	if err != nil {
		return "", err
	}
	select {
	case <-job.done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	if job.Status == JobFailed {
		return "", fmt.Errorf("%s", job.Result)
	}
	return job.Result, nil
}

// ResumeJob 在已结束句柄的保留上下文上续跑一拍（同步阻塞，与 agent 工具同感）。
func (r *Runner) ResumeJob(ctx context.Context, id, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("prompt 不能为空")
	}
	job, err := r.job(id)
	if err != nil {
		return "", err
	}
	r.jobMu.Lock()
	if job.Status == JobRunning || job.busy {
		r.jobMu.Unlock()
		return "", fmt.Errorf("子代理 %s 还在跑（先 wait_agent）", id)
	}
	job.busy = true
	r.jobMu.Unlock()
	defer func() {
		r.jobMu.Lock()
		job.busy = false
		r.jobMu.Unlock()
	}()

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	final, err := job.sub.run(ctx, prompt)
	r.jobMu.Lock()
	if err == nil {
		job.Status, job.Result = JobDone, final
	}
	r.jobMu.Unlock()
	return final, err
}

// Jobs 返回全部句柄快照（按 id 排序）。
func (r *Runner) Jobs() []JobView {
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	out := make([]JobView, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, JobView{ID: j.ID, Type: j.Type, Status: j.Status, Result: j.Result})
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i].ID) < len(out[j].ID) || (len(out[i].ID) == len(out[j].ID) && out[i].ID < out[j].ID)
	})
	return out
}

func (r *Runner) job(id string) (*Job, error) {
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return nil, fmt.Errorf("没有句柄 %q（用 list_agents 查看）", id)
	}
	return job, nil
}
