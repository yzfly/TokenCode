package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/tools"
)

// TestMain：设了 MCP_FAKE_SERVER 时，测试二进制本身就是一个假 MCP server
// （读 stdin 的 JSON-RPC 行、按协议应答）——不依赖任何外部进程。
func TestMain(m *testing.M) {
	if os.Getenv("MCP_FAKE_SERVER") == "1" {
		runFakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runFakeServer() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	reply := func(id int64, result string) {
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", id, result)
		out.Flush()
	}
	for sc.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue // 通知（initialized）没有 id，跳过
		}
		switch req.Method {
		case "initialize":
			reply(*req.ID, `{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake"}}`)
		case "tools/list":
			reply(*req.ID, `{"tools":[{"name":"echo","description":"echoes text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]}`)
		case "tools/call":
			var args struct {
				Text string `json:"text"`
			}
			json.Unmarshal(req.Params.Arguments, &args)
			if args.Text == "boom" {
				reply(*req.ID, `{"content":[{"type":"text","text":"it broke"}],"isError":true}`)
			} else {
				b, _ := json.Marshal("echo: " + args.Text)
				reply(*req.ID, `{"content":[{"type":"text","text":`+string(b)+`}]}`)
			}
		}
	}
}

func fakeConfig() ServerConfig {
	return ServerConfig{
		Command: []string{os.Args[0]},
		Env:     map[string]string{"MCP_FAKE_SERVER": "1"},
	}
}

// TestClientHandshakeAndCall 端到端验证：握手、工具发现、调用成功与 isError。
func TestClientHandshakeAndCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Start(ctx, "fake", fakeConfig())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer c.Close()

	if len(c.Tools()) != 1 || c.Tools()[0].Name != "echo" {
		t.Fatalf("tools wrong: %+v", c.Tools())
	}

	out, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != "echo: hi" {
		t.Fatalf("result wrong: %q", out)
	}

	if _, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"boom"}`)); err == nil {
		t.Fatal("expected error for isError result")
	}
}

// TestManagerRegistersTools 验证 Manager 后台连接后把工具注册进 registry，
// 命名为 mcp__<server>__<tool>，且 Execute 能打通。
func TestManagerRegistersTools(t *testing.T) {
	reg := tools.NewRegistry()
	m := NewManager(map[string]ServerConfig{"fake": fakeConfig()}, reg)
	defer m.Close()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := reg.Get("mcp__fake__echo"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tool never registered; statuses: %+v", m.Statuses())
		}
		time.Sleep(20 * time.Millisecond)
	}

	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != "ready" || sts[0].Tools != 1 {
		t.Fatalf("status wrong: %+v", sts)
	}

	out, err := reg.Execute(context.Background(), "mcp__fake__echo", json.RawMessage(`{"text":"via registry"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "echo: via registry" {
		t.Fatalf("result wrong: %q", out)
	}
}

// TestManagerFailedServer 验证起不来的 server 标记 failed、不影响返回。
func TestManagerFailedServer(t *testing.T) {
	reg := tools.NewRegistry()
	m := NewManager(map[string]ServerConfig{
		"broken": {Command: []string{"/nonexistent/binary"}},
	}, reg)
	defer m.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		sts := m.Statuses()
		if len(sts) == 1 && sts[0].State == "failed" {
			if sts[0].Err == "" {
				t.Fatal("expected error message")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never failed; statuses: %+v", sts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
