package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/yzfly/tokencode/internal/auth"
	"github.com/yzfly/tokencode/internal/catalog"
)

// runSubcommand 分发 flag 之外的子命令（auth / models）。
// 返回 handled=false 表示不是子命令，主流程继续走 TUI。
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "auth":
		return true, cmdAuth(args[1:])
	case "models":
		return true, cmdModels(args[1:])
	}
	return false, 0
}

// popularProviders 是 `auth login` 不带参数时优先展示的条目（国内 coding plan 在前）。
var popularProviders = []string{
	"kimi-for-coding", "zhipuai-coding-plan", "alibaba-coding-plan-cn",
	"minimax-cn-coding-plan", "tencent-coding-plan", "deepseek",
	"moonshotai-cn", "zai-coding-plan", "openrouter", "anthropic", "openai", "google",
}

func cmdAuth(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法：tokencode auth login [provider] | list | logout <provider>")
		return 2
	}
	switch args[0] {
	case "login":
		return cmdAuthLogin(args[1:])
	case "list":
		creds := auth.Providers()
		if len(creds) == 0 {
			fmt.Println("尚无已保存的凭据。tokencode auth login <provider> 添加。")
			return 0
		}
		fmt.Printf("已保存凭据（%s）\n", auth.Path())
		for _, p := range creds {
			fmt.Printf("  %s\n", p)
		}
		return 0
	case "logout":
		if len(args) < 2 {
			fmt.Println("用法：tokencode auth logout <provider>")
			return 2
		}
		if err := auth.Remove(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Println("已移除：", args[1])
		return 0
	default:
		fmt.Println("用法：tokencode auth login [provider] | list | logout <provider>")
		return 2
	}
}

func cmdAuthLogin(args []string) int {
	in := bufio.NewReader(os.Stdin)
	id := ""
	if len(args) > 0 {
		id = args[0]
	} else {
		fmt.Println("常用 provider（完整目录见 `tokencode models`）：")
		for _, pid := range popularProviders {
			if p, ok := catalog.Find(pid); ok {
				fmt.Printf("  %-26s %s\n", pid, p.Name)
			}
		}
		fmt.Print("\nprovider id: ")
		line, err := in.ReadString('\n')
		if err != nil {
			return 1
		}
		id = strings.TrimSpace(line)
	}
	p, ok := catalog.Find(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "error: 目录中没有 provider %q（tokencode models %s 搜索）\n", id, id)
		return 1
	}
	if p.Doc != "" {
		fmt.Printf("%s · 申请 key：%s\n", p.Name, p.Doc)
	}
	fmt.Print("API key: ")
	line, err := in.ReadString('\n')
	if err != nil {
		return 1
	}
	key := strings.TrimSpace(line)
	if key == "" {
		fmt.Fprintln(os.Stderr, "error: key 不能为空")
		return 1
	}
	if err := auth.Set(id, auth.Credential{Type: "api", Key: key}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	example := id + "/<model-id>"
	if len(p.Models) > 0 {
		example = id + "/" + p.Models[0].ID
	}
	fmt.Printf("✓ 已保存到 %s（0600）\n用法：tokencode -model %s（/model 可热切换）\n", auth.Path(), example)
	return 0
}

// cmdModels 列内置目录：无参列 provider 概览，带参先精确匹配 provider
// 列其模型，否则按子串过滤 provider。
func cmdModels(args []string) int {
	filter := ""
	if len(args) > 0 {
		filter = strings.ToLower(args[0])
	}

	if p, ok := catalog.Find(filter); ok {
		fmt.Printf("%s（%s）· 协议 %s · %s\n", p.ID, p.Name, p.Protocol, credStatus(p))
		if p.BaseURL != "" {
			fmt.Println("endpoint:", p.BaseURL)
		}
		for _, m := range p.Models {
			ctx := ""
			if m.Context > 0 {
				ctx = fmt.Sprintf(" · %dk ctx", m.Context/1000)
			}
			fmt.Printf("  %-44s %s%s\n", p.ID+"/"+m.ID, m.Name, ctx)
		}
		return 0
	}

	n := 0
	for _, p := range catalog.All() {
		if !p.Usable() {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(p.ID+" "+p.Name), filter) {
			continue
		}
		fmt.Printf("  %-30s %-28s %-9s %3d 模型 · %s\n", p.ID, p.Name, p.Protocol, len(p.Models), credStatus(p))
		n++
	}
	if n == 0 {
		fmt.Println("没有匹配的 provider。")
		return 1
	}
	fmt.Printf("\n共 %d 个 provider · tokencode models <provider-id> 看模型 · tokencode auth login <provider-id> 配 key\n", n)
	return 0
}

// credStatus 报告一个目录条目的凭据状态。
func credStatus(p catalog.Provider) string {
	if p.KeyFromEnv() != "" {
		return "✓ env"
	}
	if auth.Get(p.ID) != "" {
		return "✓ auth.json"
	}
	return "无凭据"
}
