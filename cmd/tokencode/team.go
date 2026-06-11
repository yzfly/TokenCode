package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yzfly/tokencode/internal/channel"
	"github.com/yzfly/tokencode/internal/headless"
)

// cmdTeam 实现 `tokencode team`：团队模式的成员管理。
// pair 只写 team.json 的 pending 段——真正的配对在 serve 进程里完成
// （成员在 IM 里发码），CLI 与 serve 进程由此解耦。
func cmdTeam(args []string) int {
	if len(args) == 0 {
		printTeamUsage()
		return 2
	}
	switch args[0] {
	case "pair":
		return cmdTeamPair(args[1:])
	case "list":
		return cmdTeamList()
	case "remove":
		return cmdTeamRemove(args[1:])
	default:
		printTeamUsage()
		return 2
	}
}

func printTeamUsage() {
	fmt.Println(`用法：tokencode team <子命令>
  pair -workspace <目录> [-name 备注] [-tools read,bash,...] [-model 别名] [-yolo]
        生成 8 位配对码（1 小时有效、单次有效、最多 ` + fmt.Sprint(channel.MaxPending) + ` 个待认领）
  list  列出已绑定成员与待认领配对码
  remove <channel> <user_id>  解除一条绑定`)
}

func cmdTeamPair(args []string) int {
	fs := flag.NewFlagSet("team pair", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "成员的工作空间根目录（必填，须已存在）")
	name := fs.String("name", "", "成员备注名（可选）")
	toolsFlag := fs.String("tools", "", "工具白名单，逗号分隔（默认 "+strings.Join(headless.DefaultAllowed, ",")+"）")
	model := fs.String("model", "", "该成员专属模型（可选，空=服务默认）")
	yolo := fs.Bool("yolo", false, "全工具放行（信任成员才开）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*workspace) == "" {
		fmt.Fprintln(os.Stderr, "error: -workspace 必填")
		return 2
	}
	abs, err := filepath.Abs(*workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "error: workspace %s 不存在或不是目录\n", abs)
		return 1
	}

	code, err := channel.GenCode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	store := channel.NewStore("")
	p := channel.Pending{
		Code:         code,
		Name:         strings.TrimSpace(*name),
		Workspace:    abs,
		AllowedTools: splitTools(*toolsFlag),
		Yolo:         *yolo,
		Model:        strings.TrimSpace(*model),
		ExpiresAt:    time.Now().Add(channel.PairTTL),
	}
	if err := store.AddPending(p); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	toolDesc := strings.Join(p.AllowedTools, ",")
	if *yolo {
		toolDesc = "全部（yolo）"
	} else if toolDesc == "" {
		toolDesc = strings.Join(headless.DefaultAllowed, ",") + "（默认）"
	}
	fmt.Printf(`配对码：%s
工作空间：%s
工具：%s · 有效期 1 小时 · 单次有效
让成员在 IM 里给机器人发这串码即可完成绑定（serve 进程须在跑且已配通道）。
`, code, abs, toolDesc)
	return 0
}

func cmdTeamList() int {
	store := channel.NewStore("")
	t, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(t.Bindings) == 0 && len(t.Pending) == 0 {
		fmt.Println("尚无绑定与待认领配对码。tokencode team pair -workspace <目录> 生成。")
		return 0
	}
	if len(t.Bindings) > 0 {
		fmt.Printf("已绑定成员（%s）\n", store.Path())
		for _, b := range t.Bindings {
			tools := strings.Join(b.AllowedTools, ",")
			if b.Yolo {
				tools = "yolo"
			} else if tools == "" {
				tools = "默认只读集"
			}
			name := b.Name
			if name == "" {
				name = "-"
			}
			fmt.Printf("  %-8s %-32s %-12s %s · %s\n", b.Channel, b.UserID, name, b.Workspace, tools)
		}
	}
	now := time.Now()
	live := 0
	for _, p := range t.Pending {
		if now.Before(p.ExpiresAt) {
			if live == 0 {
				fmt.Println("待认领配对码")
			}
			live++
			fmt.Printf("  %s → %s（%s 过期）\n", p.Code, p.Workspace, p.ExpiresAt.Format("15:04"))
		}
	}
	return 0
}

func cmdTeamRemove(args []string) int {
	if len(args) < 2 {
		fmt.Println("用法：tokencode team remove <channel> <user_id>")
		return 2
	}
	store := channel.NewStore("")
	ok, err := store.Remove(args[0], args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !ok {
		fmt.Printf("没有这条绑定：%s %s（tokencode team list 查看）\n", args[0], args[1])
		return 1
	}
	fmt.Printf("已解除绑定：%s %s（serve 每条消息都查绑定，下一条消息起即失效）\n", args[0], args[1])
	return 0
}
