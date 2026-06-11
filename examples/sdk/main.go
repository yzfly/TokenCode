// examples/sdk 演示 TokenCode Go SDK 的最小用法：
// 默认用 fake LLM 离线演示完整的 tool-use 回路；设置 ANTHROPIC_AUTH_TOKEN
// （或 ANTHROPIC_API_KEY）后改走真实模型。
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/yzfly/tokencode/pkg/tokencode"
)

func main() {
	opts := []tokencode.Option{
		tokencode.WithTools(tokencode.DefaultTools()),
		tokencode.WithAllowedTools("read", "bash"), // 白名单：其余工具调用将被拒绝
		tokencode.WithUsageSource("examples/sdk"),
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
		opts = append(opts, tokencode.WithLLM(fakeLLM{}, "fake-model")) // 离线演示
	}
	tc, err := tokencode.New(opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// 事件流：tool_call / tool_result / assistant_delta，最后一条恒为 result。
	err = tc.RunStream(context.Background(), "看看当前目录下有什么", func(ev tokencode.Event) {
		fmt.Printf("[%s] %s%s\n", ev.Type, ev.Name, ev.Text+ev.Result)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// fakeLLM 是离线演示用的脚本模型：先要求跑一次 `ls`，再总结收口。
type fakeLLM struct{}

func (fakeLLM) Complete(_ context.Context, req tokencode.Request) (tokencode.Response, error) {
	if len(req.Messages) == 1 { // 第一拍：发起工具调用
		return tokencode.Response{ToolUses: []tokencode.ToolUse{
			{ID: "t1", Name: "bash", Input: []byte(`{"command":"ls"}`)},
		}}, nil
	}
	return tokencode.Response{Text: "目录已列出（fake 模型演示完毕）。"}, nil
}
