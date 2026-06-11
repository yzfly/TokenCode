package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/agent"
	"github.com/yzfly/tokencode/internal/headless"
	"github.com/yzfly/tokencode/internal/llm"
	"github.com/yzfly/tokencode/internal/tools"
)

// fakeLLM 按脚本依次返回预设响应；delay 模拟慢请求（信号量测试用）。
type fakeLLM struct {
	responses []llm.Response
	delay     time.Duration
	calls     int
}

func (f *fakeLLM) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return llm.Response{}, ctx.Err()
		}
	}
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

// newTestServer 建一个 Assemble 注入 fake LLM 的 Server：model=="nope" 模拟
// 解析失败；其余每请求新建独立 agent（与生产装配同构）。
func newTestServer(maxConc int, delay time.Duration) *Server {
	return &Server{
		Version:       "test",
		MaxConcurrent: maxConc,
		Assemble: func(model string, allowed []string) (*agent.Agent, string, error) {
			if model == "nope" {
				return nil, "", errors.New(`未知模型 "nope"`)
			}
			if model == "" {
				model = "default-model"
			}
			fake := &fakeLLM{delay: delay, responses: []llm.Response{
				{ToolUses: []llm.ToolUse{{ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
				{Text: "Done.", StopReason: "end_turn"},
			}}
			reg := tools.NewRegistry(headless.GateTool(echoTool{}, headless.Allow(allowed, false)))
			return agent.New(fake, reg, model, 256), model, nil
		},
	}
}

type echoTool struct{}

func (echoTool) Name() string           { return "echo" }
func (echoTool) Description() string    { return "echo" }
func (echoTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (echoTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	return "echo ok", nil
}

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer(newTestServer(0, 0).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.OK || body.Version != "test" {
		t.Fatalf("body wrong: %+v", body)
	}
}

func TestRunSyncJSON(t *testing.T) {
	ts := httptest.NewServer(newTestServer(0, 0).Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/run", "application/json",
		strings.NewReader(`{"prompt":"go","allowed_tools":["echo"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var res headless.Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Result != "Done." || res.Model != "default-model" || res.ToolCalls != 1 || res.IsError {
		t.Fatalf("result wrong: %+v", res)
	}
}

func TestRunSSE(t *testing.T) {
	ts := httptest.NewServer(newTestServer(0, 0).Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/run", strings.NewReader(`{"prompt":"go","allowed_tools":["echo"]}`))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	// 解析 data: 帧，校验事件顺序与收尾的 result。
	var events []headless.Event
	for _, frame := range strings.Split(buf.String(), "\n\n") {
		frame = strings.TrimSpace(frame)
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		var ev headless.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &ev); err != nil {
			t.Fatalf("bad frame %q: %v", frame, err)
		}
		events = append(events, ev)
	}
	if len(events) < 3 {
		t.Fatalf("too few events: %+v", events)
	}
	last := events[len(events)-1]
	if last.Type != "result" || last.Result != "Done." || last.ToolCalls == nil || *last.ToolCalls != 1 {
		t.Fatalf("last event wrong: %+v", last)
	}
	if events[0].Type != "tool_call" || events[0].Name != "echo" {
		t.Fatalf("first event wrong: %+v", events[0])
	}
}

func TestRunUnknownModel400(t *testing.T) {
	ts := httptest.NewServer(newTestServer(0, 0).Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/run", "application/json",
		strings.NewReader(`{"prompt":"go","model":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error, "nope") {
		t.Fatalf("error message wrong: %+v", body)
	}
}

func TestRunEmptyPrompt400(t *testing.T) {
	ts := httptest.NewServer(newTestServer(0, 0).Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/run", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// 并发量大于信号量上限时全部请求仍能完成（排队不死锁）。
func TestConcurrencySemaphoreNoDeadlock(t *testing.T) {
	ts := httptest.NewServer(newTestServer(2, 20*time.Millisecond).Handler())
	defer ts.Close()

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/run", "application/json",
				strings.NewReader(`{"prompt":"go","allowed_tools":["echo"]}`))
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			var res headless.Result
			if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
				errCh <- err
				return
			}
			if res.Result != "Done." {
				errCh <- fmt.Errorf("result = %+v", res)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("requests deadlocked behind the semaphore")
	}
	close(errCh)
	for err := range errCh {
		t.Errorf("request failed: %v", err)
	}
}
