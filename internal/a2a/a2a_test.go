package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeLLM 按脚本依次返回预设响应（与 serve 包的测试 fake 同构，不烧 token）。
type fakeLLM struct {
	responses []llm.Response
	err       error
	calls     int
}

func (f *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if f.err != nil {
		return llm.Response{}, f.err
	}
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

// newTestServer 建一个 Assemble 注入 fake LLM 的 A2A Server。
// fail=true 时 LLM 恒错（FAILED 路径）。
func newTestServer(fail bool) *Server {
	return &Server{
		Version: "test",
		Assemble: func(model string, allowed []string) (*agent.Agent, string, error) {
			fake := &fakeLLM{responses: []llm.Response{{Text: "Done.", StopReason: "end_turn"}}}
			if fail {
				fake.err = errors.New("模型挂了")
			}
			return agent.New(fake, tools.NewRegistry(), "fake-model", 256), "fake-model", nil
		},
	}
}

func startServer(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	newTestServer(fail).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// rpc 发一个 JSON-RPC 请求并解析响应 envelope。
func rpc(t *testing.T, ts *httptest.Server, body string) rpcResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/a2a", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return rpcResponse{JSONRPC: out.JSONRPC, ID: out.ID, Result: out.Result, Error: out.Error}
}

// TestAgentCard 校验 card 必填字段齐全且接口 url 随请求 Host 推导。
func TestAgentCard(t *testing.T) {
	ts := startServer(t, false)

	resp, err := http.Get(ts.URL + "/.well-known/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var card struct {
		Name                string `json:"name"`
		Description         string `json:"description"`
		Version             string `json:"version"`
		SupportedInterfaces []struct {
			URL             string `json:"url"`
			ProtocolBinding string `json:"protocolBinding"`
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"supportedInterfaces"`
		Capabilities       struct{ Streaming bool }    `json:"capabilities"`
		DefaultInputModes  []string                    `json:"defaultInputModes"`
		DefaultOutputModes []string                    `json:"defaultOutputModes"`
		Skills             []struct{ ID, Name string } `json:"skills"`
		SecuritySchemes    map[string]json.RawMessage  `json:"securitySchemes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "TokenCode" || card.Description == "" || card.Version != "test" {
		t.Fatalf("card 基本字段不齐: %+v", card)
	}
	if len(card.SupportedInterfaces) != 1 {
		t.Fatalf("supportedInterfaces = %+v", card.SupportedInterfaces)
	}
	iface := card.SupportedInterfaces[0]
	if iface.URL != ts.URL+"/a2a" { // httptest 的 URL 即 http://127.0.0.1:port
		t.Fatalf("接口 url 未随 Host 推导: %q want %q", iface.URL, ts.URL+"/a2a")
	}
	if iface.ProtocolBinding != "JSONRPC" || iface.ProtocolVersion != "1.0" {
		t.Fatalf("接口声明错: %+v", iface)
	}
	if !card.Capabilities.Streaming || len(card.Skills) == 0 || card.Skills[0].ID != "code" {
		t.Fatalf("capabilities/skills 错: %+v", card)
	}
	if len(card.DefaultInputModes) == 0 || card.DefaultInputModes[0] != "text/plain" ||
		len(card.DefaultOutputModes) == 0 {
		t.Fatalf("modes 错: %+v", card)
	}
	if _, ok := card.SecuritySchemes["bearer"]; !ok {
		t.Fatalf("securitySchemes 缺 bearer: %+v", card.SecuritySchemes)
	}
}

// sendResult 解析 SendMessage 响应 result 的 oneof。
type sendResult struct {
	Message *Message `json:"message"`
	Task    *Task    `json:"task"`
}

func decodeSend(t *testing.T, r rpcResponse) sendResult {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("意外的 RPC 错误: %+v", r.Error)
	}
	var out sendResult
	if err := json.Unmarshal(r.Result.(json.RawMessage), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSendMessage 验证 v1 SendMessage 全链路：fake LLM 跑完回 ROLE_AGENT message。
func TestSendMessage(t *testing.T) {
	ts := startServer(t, false)

	r := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"SendMessage",
		"params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`)
	out := decodeSend(t, r)
	if out.Message == nil {
		t.Fatalf("缺 message: %+v", out)
	}
	m := out.Message
	if m.Role != "ROLE_AGENT" || len(m.Parts) != 1 || m.Parts[0].Text != "Done." {
		t.Fatalf("message 错: %+v", m)
	}
	if m.MessageID == "" || m.ContextID == "" || m.TaskID == "" {
		t.Fatalf("缺 id 字段: %+v", m)
	}
}

// TestSendMessageAlias 验证 0.x 别名 message/send 与 v1 等价（含 0.x 的 kind 字段）。
func TestSendMessageAlias(t *testing.T) {
	ts := startServer(t, false)

	r := rpc(t, ts, `{"jsonrpc":"2.0","id":"a","method":"message/send",
		"params":{"message":{"messageId":"m1","role":"user","parts":[{"kind":"text","text":"hi"}]}}}`)
	out := decodeSend(t, r)
	if out.Message == nil || out.Message.Parts[0].Text != "Done." {
		t.Fatalf("别名 message/send 不等价: %+v", out)
	}
}

// TestSendMessageFailed 验证 LLM 错误回 Task 形态 FAILED（实现选定的错误形态）。
func TestSendMessageFailed(t *testing.T) {
	ts := startServer(t, true)

	r := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"SendMessage",
		"params":{"message":{"messageId":"m1","parts":[{"text":"hi"}]}}}`)
	out := decodeSend(t, r)
	if out.Task == nil || out.Task.Status.State != stateFailed {
		t.Fatalf("应回 FAILED task: %+v", out)
	}
	if out.Task.Status.Message == nil || out.Task.Status.Message.Parts[0].Text == "" {
		t.Fatalf("FAILED task 应留存错误文本: %+v", out.Task)
	}
}

// TestGetTask 验证 SendMessage 登记的 task 可查，未知 id 回 -32001。
func TestGetTask(t *testing.T) {
	ts := startServer(t, false)

	r := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"SendMessage",
		"params":{"message":{"messageId":"m1","parts":[{"text":"hi"}]}}}`)
	taskID := decodeSend(t, r).Message.TaskID

	r = rpc(t, ts, `{"jsonrpc":"2.0","id":2,"method":"GetTask","params":{"id":"`+taskID+`"}}`)
	if r.Error != nil {
		t.Fatalf("GetTask 错误: %+v", r.Error)
	}
	var tk Task
	if err := json.Unmarshal(r.Result.(json.RawMessage), &tk); err != nil {
		t.Fatal(err)
	}
	if tk.ID != taskID || tk.Status.State != stateCompleted {
		t.Fatalf("task 错: %+v", tk)
	}
	if tk.Status.Message == nil || tk.Status.Message.Parts[0].Text != "Done." {
		t.Fatalf("终态未留存最终 message: %+v", tk)
	}

	// 别名 + 未知 id → -32001 TaskNotFound。
	r = rpc(t, ts, `{"jsonrpc":"2.0","id":3,"method":"tasks/get","params":{"id":"nope"}}`)
	if r.Error == nil || r.Error.Code != codeTaskNotFound {
		t.Fatalf("应回 -32001: %+v", r.Error)
	}
}

// TestSendStreamingMessage 解析 SSE 帧：statusUpdate(WORKING) → artifactUpdate
// → statusUpdate(COMPLETED) 顺序，且每帧都是合法 JSON-RPC envelope。
func TestSendStreamingMessage(t *testing.T) {
	ts := startServer(t, false)

	resp, err := http.Post(ts.URL+"/a2a", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"SendStreamingMessage",
			"params":{"message":{"messageId":"m1","parts":[{"text":"hi"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var raw strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	type frame struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			StatusUpdate *struct {
				TaskID string     `json:"taskId"`
				Final  bool       `json:"final"`
				Status TaskStatus `json:"status"`
			} `json:"statusUpdate"`
			ArtifactUpdate *struct {
				TaskID   string `json:"taskId"`
				Append   bool   `json:"append"`
				Artifact struct {
					ArtifactID string `json:"artifactId"`
					Parts      []Part `json:"parts"`
				} `json:"artifact"`
			} `json:"artifactUpdate"`
		} `json:"result"`
	}
	var frames []frame
	for _, chunk := range strings.Split(raw.String(), "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if !strings.HasPrefix(chunk, "data: ") {
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(strings.TrimPrefix(chunk, "data: ")), &f); err != nil {
			t.Fatalf("坏帧 %q: %v", chunk, err)
		}
		if f.JSONRPC != "2.0" || string(f.ID) != "7" {
			t.Fatalf("envelope 错: %+v", f)
		}
		frames = append(frames, f)
	}
	if len(frames) < 3 {
		t.Fatalf("帧太少: %d", len(frames))
	}
	first, last := frames[0], frames[len(frames)-1]
	if first.Result.StatusUpdate == nil || first.Result.StatusUpdate.Status.State != stateWorking {
		t.Fatalf("首帧应为 WORKING statusUpdate: %+v", first)
	}
	if last.Result.StatusUpdate == nil || last.Result.StatusUpdate.Status.State != stateCompleted || !last.Result.StatusUpdate.Final {
		t.Fatalf("末帧应为 COMPLETED 终态: %+v", last)
	}
	mid := frames[1]
	if mid.Result.ArtifactUpdate == nil || !mid.Result.ArtifactUpdate.Append ||
		mid.Result.ArtifactUpdate.Artifact.Parts[0].Text != "Done." {
		t.Fatalf("中间帧应为 append artifactUpdate: %+v", mid)
	}
	if first.Result.StatusUpdate.TaskID == "" || first.Result.StatusUpdate.TaskID != mid.Result.ArtifactUpdate.TaskID {
		t.Fatal("各帧 taskId 不一致")
	}

	// 0.x 别名 message/stream 同样走 SSE。
	resp2, err := http.Post(ts.URL+"/a2a", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":8,"method":"message/stream",
			"params":{"message":{"messageId":"m2","parts":[{"text":"hi"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if ct := resp2.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("别名 stream content-type = %q", ct)
	}
}

// TestUnknownMethod 验证未实现的方法回 -32004 UnsupportedOperationError。
func TestUnknownMethod(t *testing.T) {
	ts := startServer(t, false)
	r := rpc(t, ts, `{"jsonrpc":"2.0","id":1,"method":"ListTasks","params":{}}`)
	if r.Error == nil || r.Error.Code != codeUnsupportedOp {
		t.Fatalf("应回 -32004: %+v", r.Error)
	}
}

// TestBadJSON 验证坏 JSON 回 -32700、非 2.0 envelope 回 -32600。
func TestBadJSON(t *testing.T) {
	ts := startServer(t, false)

	r := rpc(t, ts, `{not json`)
	if r.Error == nil || r.Error.Code != codeParseError {
		t.Fatalf("应回 -32700: %+v", r.Error)
	}
	r = rpc(t, ts, `{"id":1,"method":"SendMessage"}`) // 缺 jsonrpc:"2.0"
	if r.Error == nil || r.Error.Code != codeInvalidRequest {
		t.Fatalf("应回 -32600: %+v", r.Error)
	}
}
