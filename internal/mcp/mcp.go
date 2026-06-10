// Package mcp 是 Model Context Protocol 的最小 stdio client：
// 拉起 server 子进程，走 JSON-RPC 2.0（每行一条消息）完成 initialize 握手、
// tools/list 发现、tools/call 调用。外部 server 的工具以 mcp__<server>__<tool>
// 注册进工具注册表，对 agent 循环与普通工具无差别。
//
// 设计铁律（来自架构图 §1.2）：连接在后台进行、失败不阻塞启动；
// 退出时硬 kill 子进程（部分 server 不理 SIGTERM）。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/yzfly/tokencode/internal/tools"
)

// protocolVersion 是握手时声明的 MCP 协议版本。
const protocolVersion = "2024-11-05"

// ServerConfig 是一个 stdio MCP server 的配置（config.json 的 mcp 字段值）。
type ServerConfig struct {
	Command []string          `json:"command"`       // argv，必填
	Env     map[string]string `json:"env,omitempty"` // 追加到子进程环境
}

// ToolDef 是 server 暴露的一个工具定义。
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Client 是与单个 server 的连接。请求经互斥的 id 分配 + reader goroutine
// 路由响应，支持并发调用与 ctx 超时。
type Client struct {
	name string
	cmd  *exec.Cmd

	writeMu sync.Mutex
	stdin   io.Writer

	pendMu  sync.Mutex
	pending map[int64]chan json.RawMessage
	nextID  int64

	tools []ToolDef
}

// rpcRequest / rpcResponse 是 JSON-RPC 2.0 线上形态。
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"` // nil = notification
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Start 拉起 server 并完成握手与工具发现。调用方应放在 goroutine 里
// （连接慢或失败都不该阻塞主流程）。
func Start(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("mcp %s: command 为空", name)
	}
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil // server 的日志不进我们的终端（TUI 占着屏幕）
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp %s: 启动失败: %w", name, err)
	}

	c := &Client{name: name, cmd: cmd, stdin: stdin, pending: map[int64]chan json.RawMessage{}}
	go c.readLoop(stdout)

	// initialize → initialized → tools/list，整个握手受 ctx 限时。
	var initRes json.RawMessage
	initRes, err = c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tokencode", "version": "0.1"},
	})
	_ = initRes
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialize: %w", name, err)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: initialized: %w", name, err)
	}

	res, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: tools/list: %w", name, err)
	}
	var listed struct {
		Tools []ToolDef `json:"tools"`
	}
	if err := json.Unmarshal(res, &listed); err != nil {
		c.Close()
		return nil, fmt.Errorf("mcp %s: 解析工具列表: %w", name, err)
	}
	c.tools = listed.Tools
	return c, nil
}

// Name 返回 server 名。
func (c *Client) Name() string { return c.name }

// Tools 返回 server 暴露的工具定义。
func (c *Client) Tools() []ToolDef { return c.tools }

// Close 硬终止子进程（不等 SIGTERM 礼貌退出——有的 server 不理）。
func (c *Client) Close() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
}

// CallTool 调用 server 的一个工具，返回拼接后的文本内容。
func (c *Client) CallTool(ctx context.Context, tool string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", fmt.Errorf("mcp %s: 解析工具结果: %w", c.name, err)
	}
	var sb strings.Builder
	for _, blk := range out.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		} else {
			fmt.Fprintf(&sb, "[%s content]", blk.Type)
		}
	}
	if out.IsError {
		return "", fmt.Errorf("%s", sb.String())
	}
	return sb.String(), nil
}

// readLoop 把响应按 id 路由给等待者；通知与 server 发起的请求直接丢弃（v1）。
func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var resp rpcResponse
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil || resp.ID == nil {
			continue
		}
		c.pendMu.Lock()
		ch, ok := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.pendMu.Unlock()
		if ok {
			b, _ := json.Marshal(resp)
			ch <- b
		}
	}
	// EOF：唤醒所有等待者，避免悬挂。
	c.pendMu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pendMu.Unlock()
}

// call 发请求并等响应（ctx 控制超时/取消）。
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.pendMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.pendMu.Unlock()

	if err := c.send(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
		return nil, ctx.Err()
	case raw, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("mcp %s: 连接已断开", c.name)
		}
		var resp rpcResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp %s: %s", c.name, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// notify 发一条无 id 的通知。
func (c *Client) notify(method string, params any) error {
	return c.send(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) send(req rpcRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// ---- Manager：多 server 的非阻塞装配 ----

// Status 是一个 server 的当前状态（/mcp 展示用）。
type Status struct {
	Name  string
	State string // connecting | ready | failed
	Err   string
	Tools int
}

// Manager 管全部已配置 server：后台连接、状态查询、工具注册、重连、统一关闭。
type Manager struct {
	reg     *tools.Registry
	configs map[string]ServerConfig

	mu      sync.Mutex
	clients map[string]*Client
	status  map[string]*Status
}

// NewManager 后台拉起全部 server（每个一条 goroutine，30s 握手超时），
// 连上即把工具注册进 reg。立即返回，绝不阻塞启动。
func NewManager(configs map[string]ServerConfig, reg *tools.Registry) *Manager {
	m := &Manager{
		reg:     reg,
		configs: configs,
		clients: map[string]*Client{},
		status:  map[string]*Status{},
	}
	for name := range configs {
		m.status[name] = &Status{Name: name, State: "connecting"}
		go m.connect(name)
	}
	return m
}

// connect 拉起一个 server 并注册其工具（goroutine 体）。
func (m *Manager) connect(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Start(ctx, name, m.configs[name])
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[name]
	if err != nil {
		st.State, st.Err = "failed", err.Error()
		return
	}
	m.clients[name] = c
	st.State, st.Err, st.Tools = "ready", "", len(c.Tools())
	for _, def := range c.Tools() {
		m.reg.Add(&serverTool{c: c, def: def})
	}
}

// Reconnect 重连一个 server（/mcp reconnect <name>）：先硬断旧连接，
// 再后台重新握手；工具重名覆盖注册，旧条目自然指向新连接。
func (m *Manager) Reconnect(name string) error {
	m.mu.Lock()
	st, ok := m.status[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp: 未配置 server %q", name)
	}
	if old := m.clients[name]; old != nil {
		old.Close()
		delete(m.clients, name)
	}
	st.State, st.Err, st.Tools = "connecting", "", 0
	m.mu.Unlock()
	go m.connect(name)
	return nil
}

// Statuses 返回各 server 状态（名字序与配置无关，调用方自行排序）。
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.status))
	for _, st := range m.status {
		out = append(out, *st)
	}
	return out
}

// Close 关闭全部连接。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.clients {
		c.Close()
	}
}

// serverTool 把 MCP 工具适配成 agent 工具。
type serverTool struct {
	c   *Client
	def ToolDef
}

func (t *serverTool) Name() string { return "mcp__" + t.c.name + "__" + t.def.Name }
func (t *serverTool) Description() string {
	return fmt.Sprintf("[MCP %s] %s", t.c.name, t.def.Description)
}
func (t *serverTool) Schema() map[string]any {
	if t.def.InputSchema == nil {
		return map[string]any{"type": "object"}
	}
	return t.def.InputSchema
}
func (t *serverTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return t.c.CallTool(ctx, t.def.Name, input)
}
