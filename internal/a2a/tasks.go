package a2a

import (
	"context"
	"sync"
)

// A2A v1 任务状态（SCREAMING_SNAKE_CASE）。v0 状态机：
// WORKING → COMPLETED / FAILED / CANCELED（请求即跑，SUBMITTED 一闪而过不留观测窗口，
// put 直接登记为 WORKING）。终态只写一次：CancelTask 先标 CANCELED 后，
// run 因 ctx 取消而失败也不会把它改写成 FAILED。
const (
	stateWorking   = "TASK_STATE_WORKING"
	stateCompleted = "TASK_STATE_COMPLETED"
	stateFailed    = "TASK_STATE_FAILED"
	stateCanceled  = "TASK_STATE_CANCELED"

	roleAgent = "ROLE_AGENT"
)

// maxTasks 是内存 task 上限：超出按登记顺序淘汰最旧的（FIFO 而非 LRU——
// task 是一次性产物，几乎不会被反复查询，FIFO 足够且省一份访问记账）。
const maxTasks = 1000

// entry 是一条在册任务：task 快照 + 取消该次 run 的钩子。
type entry struct {
	task   Task
	cancel context.CancelFunc
}

// store 是内存 task 表（v0 不落盘：A2A 任务的生命周期不长于进程）。
type store struct {
	mu    sync.Mutex
	m     map[string]*entry
	order []string // 登记顺序，FIFO 淘汰用
	max   int
}

func newStore(max int) *store {
	return &store{m: make(map[string]*entry), max: max}
}

// put 登记新任务；超上限先淘汰最旧的（顺带 cancel，避免泄漏在跑的 ctx）。
func (s *store) put(t Task, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.order) >= s.max {
		oldest := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.m[oldest]; ok {
			e.cancel()
			delete(s.m, oldest)
		}
	}
	s.m[t.ID] = &entry{task: t, cancel: cancel}
	s.order = append(s.order, t.ID)
}

// get 返回任务快照。
func (s *store) get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return Task{}, false
	}
	return e.task, true
}

// finish 写入 run 的结局并返回最终快照：isErr 定 COMPLETED/FAILED，
// 但若已被 CancelTask 标为终态则保持原样（终态只写一次）。
// 最终 message 两种结局都留存——FAILED 的错误文本同样有诊断价值。
func (s *store) finish(id string, isErr bool, msg *Message) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok { // 已被淘汰：只能就地拼一个快照返回，不再登记
		state := stateCompleted
		if isErr {
			state = stateFailed
		}
		return Task{ID: id, Status: TaskStatus{State: state, Message: msg}}
	}
	if terminal(e.task.Status.State) {
		return e.task
	}
	e.task.Status.State = stateCompleted
	if isErr {
		e.task.Status.State = stateFailed
	}
	e.task.Status.Message = msg
	return e.task
}

// cancel 取消任务：触发 ctx cancel 并标 CANCELED（已是终态则幂等返回现状）。
func (s *store) cancel(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return Task{}, false
	}
	e.cancel()
	if !terminal(e.task.Status.State) {
		e.task.Status.State = stateCanceled
	}
	return e.task, true
}

func terminal(state string) bool {
	switch state {
	case stateCompleted, stateFailed, stateCanceled:
		return true
	}
	return false
}
