// Package a2a 实现 A2A（Agent-to-Agent）协议 v1 的最小服务端子集，让 TokenCode
// 可以被其他 agent 发现（agent card）、对话（SendMessage）、触发并跟踪（task）。
//
// 范围（v0，对应 docs/research/2026-06-11-a2a-protocol.md 的「最小合规子集」）：
//   - GET /.well-known/agent-card.json：发现端点，url 随请求 Host 推导；
//   - POST /a2a：JSON-RPC 2.0 单端点，v1 PascalCase 方法 + 0.x 别名一并接受
//     （生态大量客户端仍在 0.x，官方 a2a-go 也有 a2acompat 同款兼容层）；
//   - SendMessage 阻塞跑完回 message；SendStreamingMessage 走 SSE；
//   - 内存 task store 支撑 GetTask / CancelTask，带上限防泄漏。
//
// 安全边界沿用 serve：v0 无鉴权、默认仅回环。card 里仍声明 Bearer scheme——
// 这是给将来加 token 校验的留位（外部 agent 拿 card 即知道该怎么带凭证），
// 服务端当前不校验 Authorization 头。A2A-Version 头同样忽略（不校验不报错）。
package a2a

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/headless"
)

// Server 是 A2A 端点的装配点：serve 经 Mount 回调把这里的路由挂上自己的 mux。
type Server struct {
	Version       string
	MaxConcurrent int // 同时在跑的 run 上限；≤0 用默认 8
	// Assemble 与 serve.Server.Assemble 同签名：每次请求装配独立 agent。
	// A2A 消息不携带模型与工具白名单，恒用默认模型 + headless.DefaultAllowed
	//（将来可在 card skills 或扩展参数里开放，留位）。
	Assemble func(model string, allowed []string) (*agent.Agent, string, error)

	once  sync.Once
	sem   chan struct{} // Mount 注入拿不到 serve 的信号量，自带一个同默认值的
	tasks *store
}

// Register 把 A2A 路由注册到 mux 上（与 webui.Server.Register 同范式）。
func (s *Server) Register(mux *http.ServeMux) {
	s.once.Do(func() {
		n := s.MaxConcurrent
		if n <= 0 {
			n = 8
		}
		s.sem = make(chan struct{}, n)
		s.tasks = newStore(maxTasks)
	})
	mux.HandleFunc("GET /.well-known/agent-card.json", s.handleCard)
	mux.HandleFunc("POST /a2a", s.handleRPC)
}

// ---- Agent Card（发现端点）----

// handleCard 返回 AgentCard。url 从请求 Host 推导：客户端从哪个地址拿到 card，
// 就让它往哪个地址发 JSON-RPC（绑非回环、走端口转发时都不用配置）。
func (s *Server) handleCard(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	card := map[string]any{
		"name":        "TokenCode",
		"description": "快为第一性的团队 Agent 引擎：单二进制、可被 IM / HTTP / A2A 触发的编码与自动化 agent。",
		"version":     s.Version,
		"supportedInterfaces": []map[string]any{{
			"url":             scheme + "://" + r.Host + "/a2a",
			"protocolBinding": "JSONRPC",
			"protocolVersion": "1.0",
		}},
		"capabilities":       map[string]any{"streaming": true},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"skills": []map[string]any{{
			"id":          "code",
			"name":        "Coding Agent",
			"description": "在工作目录内读码/写码/跑命令/联网调研的编码 agent",
			"tags":        []string{"coding", "golang"},
		}},
		// 声明 Bearer 供将来鉴权用；v0 不校验（默认仅回环，与 serve 边界一致）。
		"securitySchemes": map[string]any{
			"bearer": map[string]any{
				"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(card)
}

// ---- JSON-RPC 2.0 分发器 ----

// JSON-RPC 标准错误码 + A2A 专属错误码（规范 §5.4 的映射表）。
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeTaskNotFound   = -32001 // TaskNotFoundError
	codeUnsupportedOp  = -32004 // UnsupportedOperationError
)

// rpcRequest 是 JSON-RPC 2.0 请求。id 保持 RawMessage 原样回显（string/number 都合法）。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcResponse 是 JSON-RPC 2.0 响应（result 与 error 二选一）。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ---- A2A 数据对象（按 v1 proto 的 JSON 命名，字段拼写对照官方 a2a-go）----

// Part 是消息片段。v0 只支持文本；0.x 客户端会多带 kind:"text"，解码兼容、
// 编码不回写（v1 删了判别字段）。
type Part struct {
	Text string `json:"text"`
}

// Message 是一条 A2A 消息。
type Message struct {
	MessageID string `json:"messageId"`
	Role      string `json:"role"` // ROLE_USER / ROLE_AGENT
	Parts     []Part `json:"parts"`
	ContextID string `json:"contextId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
}

// TaskStatus 是任务状态（终态附带最终 agent 消息）。
type TaskStatus struct {
	State   string   `json:"state"`
	Message *Message `json:"message,omitempty"`
}

// Task 是任务对象。
type Task struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId,omitempty"`
	Status    TaskStatus `json:"status"`
}

// sendParams 是 SendMessage / SendStreamingMessage 的 params。
type sendParams struct {
	Message Message `json:"message"`
}

// idParams 是 GetTask / CancelTask 的 params。
type idParams struct {
	ID string `json:"id"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeRPCError(w, nil, codeParseError, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, req.ID, codeInvalidRequest, "不是合法的 JSON-RPC 2.0 请求")
		return
	}

	// v1 PascalCase 方法 + 0.x 别名（message/send 等）一并接受，见包注释。
	switch req.Method {
	case "SendMessage", "message/send":
		s.handleSend(w, r, req)
	case "SendStreamingMessage", "message/stream":
		s.handleSendStreaming(w, r, req)
	case "GetTask", "tasks/get":
		s.handleGetTask(w, req)
	case "CancelTask", "tasks/cancel":
		s.handleCancelTask(w, req)
	default:
		writeRPCError(w, req.ID, codeUnsupportedOp, "方法不支持: "+req.Method)
	}
}

// promptOf 把 parts[].text 拼接为 prompt（空片段跳过，换行衔接）。
func promptOf(m Message) string {
	var texts []string
	for _, p := range m.Parts {
		if strings.TrimSpace(p.Text) != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// prepare 解析 send 参数、过信号量、装配 agent 并登记 task（SUBMITTED→WORKING）。
// 返回 ok=false 时错误响应已写出。
func (s *Server) prepare(w http.ResponseWriter, r *http.Request, req rpcRequest) (tk Task, ag *agent.Agent, model string, runCtx context.Context, done func(), ok bool) {
	var p sendParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPCError(w, req.ID, codeInvalidParams, "params 解析失败: "+err.Error())
		return
	}
	prompt := promptOf(p.Message)
	if prompt == "" {
		writeRPCError(w, req.ID, codeInvalidParams, "message.parts 没有非空文本")
		return
	}

	// 信号量语义与 serve 一致：满了排队，客户端断开即放弃。
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	release := func() { <-s.sem }

	ag, model, err := s.Assemble("", headless.DefaultAllowed)
	if err != nil {
		release()
		writeRPCError(w, req.ID, codeInternalError, "装配 agent 失败: "+err.Error())
		return
	}

	contextID := p.Message.ContextID
	if contextID == "" {
		contextID = newUUID()
	}
	tk = Task{ID: newUUID(), ContextID: contextID, Status: TaskStatus{State: stateWorking}}
	ctx, cancel := context.WithCancel(r.Context())
	s.tasks.put(tk, cancel) // 登记后 GetTask 可查、CancelTask 可取消
	done = func() { cancel(); release() }
	return tk, ag, model, ctx, done, true
}

// handleSend 实现 SendMessage：阻塞跑完一个 turn，成功回 {"message":{...}}。
// 失败回 Task 形态 FAILED（而非 JSON-RPC -32603）：模型/网络错误是「任务没跑成」
// 不是「协议层坏了」，Task 终态可被 GetTask 复查，也与流式的 FAILED 终态一致。
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	tk, ag, model, ctx, done, ok := s.prepare(w, r, req)
	if !ok {
		return
	}
	defer done()

	var p sendParams
	_ = json.Unmarshal(req.Params, &p) // prepare 已验证过，此处不会失败
	res := headless.Run(ctx, ag, model, promptOf(p.Message), nil)

	msg := &Message{
		MessageID: newUUID(),
		Role:      roleAgent,
		Parts:     []Part{{Text: res.Result}},
		ContextID: tk.ContextID,
		TaskID:    tk.ID,
	}
	final := s.tasks.finish(tk.ID, res.IsError, msg)
	if final.Status.State == stateCompleted {
		writeRPCResult(w, req.ID, map[string]any{"message": msg})
		return
	}
	writeRPCResult(w, req.ID, map[string]any{"task": final}) // FAILED / CANCELED
}

// handleSendStreaming 实现 SendStreamingMessage：SSE，每条 data: 一个 JSON-RPC
// envelope，result 为 StreamResponse oneof。事件映射：
//
//	开跑          → statusUpdate(TASK_STATE_WORKING)
//	assistant_delta → artifactUpdate(append:true)
//	result        → statusUpdate(COMPLETED/FAILED/CANCELED) 后关流
//
// tool_call / tool_result v0 不外发：A2A 没有对应的标准事件位（塞 metadata
// 各家不互通），对外只暴露文本产物与终态。
func (s *Server) handleSendStreaming(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeRPCError(w, req.ID, codeInternalError, "底层连接不支持流式输出")
		return
	}
	tk, ag, model, ctx, done, ok := s.prepare(w, r, req)
	if !ok {
		return
	}
	defer done()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(result any) {
		b, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}

	emit(map[string]any{"statusUpdate": map[string]any{
		"taskId": tk.ID, "contextId": tk.ContextID,
		"status": TaskStatus{State: stateWorking},
	}})

	var p sendParams
	_ = json.Unmarshal(req.Params, &p)
	artifactID := newUUID()
	var res headless.Result
	res = headless.Run(ctx, ag, model, promptOf(p.Message), func(ev headless.Event) {
		if ev.Type != "assistant_delta" { // tool_* 与 result 不在此外发，见函数注释
			return
		}
		emit(map[string]any{"artifactUpdate": map[string]any{
			"taskId": tk.ID, "contextId": tk.ContextID, "append": true,
			"artifact": map[string]any{"artifactId": artifactID, "parts": []Part{{Text: ev.Text}}},
		}})
	})

	msg := &Message{
		MessageID: newUUID(),
		Role:      roleAgent,
		Parts:     []Part{{Text: res.Result}},
		ContextID: tk.ContextID,
		TaskID:    tk.ID,
	}
	final := s.tasks.finish(tk.ID, res.IsError, msg)
	emit(map[string]any{"statusUpdate": map[string]any{
		"taskId": tk.ID, "contextId": tk.ContextID, "final": true,
		"status": final.Status,
	}})
	// 发完终态即返回，连接由 http.Server 收尾关闭（与 serve.runSSE 同模式）。
}

func (s *Server) handleGetTask(w http.ResponseWriter, req rpcRequest) {
	var p idParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, codeInvalidParams, "params 需要 id 字段")
		return
	}
	tk, ok := s.tasks.get(p.ID)
	if !ok {
		writeRPCError(w, req.ID, codeTaskNotFound, "task 不存在: "+p.ID)
		return
	}
	writeRPCResult(w, req.ID, tk)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, req rpcRequest) {
	var p idParams
	if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == "" {
		writeRPCError(w, req.ID, codeInvalidParams, "params 需要 id 字段")
		return
	}
	tk, ok := s.tasks.cancel(p.ID)
	if !ok {
		writeRPCError(w, req.ID, codeTaskNotFound, "task 不存在: "+p.ID)
		return
	}
	writeRPCResult(w, req.ID, tk)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	// JSON-RPC 错误仍回 HTTP 200：错误语义在 envelope 里，这是 JSON-RPC 绑定的惯例。
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// newUUID 生成 UUID v4（crypto/rand，零依赖；与 session 包的随机 ID 同源做法）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("a2a: crypto/rand 不可用: " + err.Error()) // 系统级故障，无意义降级
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
