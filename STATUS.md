# TokenCode 当前状态

> 此刻这个 Agent 是什么、到哪一步了。5 分钟看懂版。
> 更新于 2026-06-10。深一步看：[`ROADMAP.md`](ROADMAP.md)（整张图）、[`devlog/`](devlog/)（每天脚印）。

## 一句话

一个跑在终端里的**单 agent 编码助手**：你说一句话，它用 read/write/edit/bash 四个工具读改文件、跑命令，循环直到把活干完，配一套 Bubble Tea TUI。已内置**多模型协议转换层**（anthropic / openai-chat 双协议，config 注册任意 provider）与**心跳 + 自动做梦**（agent 长活机制）。**并行（项目的核心卖点）尚未开始**——当前是为并行准备的「底座单元」。

## 路线图位置

- 阶段 0 · 单 agent MVP 底座 — **已完成**
- 阶段 1 · 交互外壳 TUI — **已完成 / 打磨中**
- 阶段 2 · A·横向爆破（竞赛）— **下一个重点，未开始**
- 阶段 3 · B·协作 + 工作区权威 — 未开始

## 现在能做什么（能力清单）

- 自然语言驱动的 tool-use 循环：消息 → LLM(带工具) → `tool_use` → 执行 → `tool_result` 回灌 → 循环 → 结束。
- 四个工具：`read`（只读免确认）/ `write` / `edit` / `bash`。
- **多模型**：内置协议转换层，`anthropic` 与 `openai-chat` 两种协议 codec；`~/.config/tokencode/config.json` 注册 provider（DeepSeek/Kimi/Qwen/OpenRouter/Ollama……），`-model 别名` 或 `-model provider/model-id` 切换；无 config 时默认经 DeepSeek 接入（`deepseek-v4-pro[1m]`），行为与之前一致。
- **Event 即拍**：用户消息、心跳、梦醒是同一个 `Event` 原语的不同来源，agent 是消费 `chan Event` 的 actor，所有 turn 串行（单写者）。
- **心跳**（`-heartbeat 30m` 显式开启）：三级短路省 token——L0 本地检查零 token → L1 空转回哨兵 `HEARTBEAT_OK` 即从历史剔除 → L2 真有事才完整跑；非交互 turn 只读放行、写类拒绝。
- **自动做梦**：空闲 ∧ 有料双条件触发，独立 goroutine 调一次 LLM 把会话压缩成 `.tokencode/memory.md`，system prompt 注入「长期记忆」——重启也记得。
- Bubble Tea TUI（alt-screen + viewport）：glamour markdown 每条渲染一次进缓存、DeepSeek 蓝主题、亮/暗自适应、鼠标滚轮滚动、字符 logo + 欢迎卡、等待时 spinner。
- 权限三模式：**plan**（只读）/ **review**（逐次 y/n/a 确认，默认）/ **yolo**（全放行）；Shift+Tab 循环或 `/plan` `/review` `/yolo` 切换。
- 跑动中 Ctrl-C 打断当前轮、回提示符；非 tty（管道/重定向）自动退化为纯文本。

## 架构概览（各包职责）

```
cmd/tokencode/main.go      解析 flag/env/config → 按协议构造 client → tui.Run
internal/llm/              LLM interface（统一 IR）+ anthropic / openai 两个协议 codec
internal/config/           模型注册表（providers/models/default_model，JSON）
internal/agent/            Agent：对话状态 + Run/Serve（Event actor）+ Snapshot
internal/pulse/            心跳（Ticker + L0 短路）+ 做梦（Dreamer → memory.md）
internal/tools/            Tool interface + Registry + read/write/edit/bash
internal/tui/              Bubble Tea 外壳（alt-screen + viewport）
  run.go      启动 program + Serve/Pulse goroutine + 非 tty 回退
  model.go    Bubble Tea Model：textarea + spinner + 状态栏 + 历史 + 确认
  messages.go msg 类型 + bridge（agent.UI 回调 → program.Send）
  theme.go    DeepSeek 配色（lipgloss + glamour，AdaptiveColor 亮暗两套）
  markdown.go renderMarkdown(md, width)
  perms.go    加锁的权限状态 + decide() 裁决（+ perms_test.go）
  logo.go     字符 logo + 欢迎卡（embed 两版 .txt）
  render.go   保留的纯函数 oneLine/compactJSON/interpretConfirmKey（+ render_test.go）
```

**一条要记住的铁律**：UI 回调跑在 worker goroutine 里、只 `program.Send` 原始数据；**所有渲染在 `Update` 单线程做**（它知道当前宽度）。这是这版 TUI 不卡的结构性原因。

`LLM` 是 interface——`Agent` 不关心背后是真 HTTP 还是 fake，所以整条 agent 循环能在不烧 token 的情况下被完整测过。

## 依赖与构建

```bash
# 环境变量（经 DeepSeek 接入）
export ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
export ANTHROPIC_AUTH_TOKEN=<你的 DeepSeek API Key>
export ANTHROPIC_MODEL=deepseek-v4-pro[1m]

# 编出二进制再跑（推荐）。go run 每次重编译较慢，TUI 启动有可感延迟。
go build -o bin/tokencode ./cmd/tokencode && ./bin/tokencode

# 也可直接 go run（慢）
go run ./cmd/tokencode

# 测试 / 静态检查
go test ./...   # agent 循环 / llm 协议(httptest) / tools / tui 纯函数 + perms，全绿，不烧 token
go vet ./...
```

常用 flag：`-model`、`-base-url`、`-max-tokens`（默认 4096）、`-yolo`（初始 yolo 模式）、`-theme auto|light|dark`、`-heartbeat 30m`（心跳，默认关闭）。

**依赖**：阶段 0 是零第三方依赖；阶段 1 引入了 charmbracelet 那套（bubbletea / bubbles / glamour / lipgloss）+ `golang.org/x/term` 及一长串间接依赖。`go.mod` 现在有 require。这是为 TUI 观感有意识付的账。

## 当前已知限制

- **不流式**：openai/anthropic codec 都只有非流式 `Complete`（流式在后续路线上），TUI 用 spinner 补「活着」感。
- **无终端原生 scrollback**：alt-screen 的代价，滚动靠 viewport + 滚轮；想找回原生滚动得换 inline 模式并解决其 resize 残留。
- **历史不持久化**：退出即丢。
- **会话不持久化**：没有 JSONL 树/分支。
- **做梦 v1 用主模型**：`DreamModel` 便宜档配置留位未接。
- **plain（非 tty）模式心跳不生效**：runPlain 同步调 Run，与 actor 并行会破坏单写者不变量。
- **没有并行**：核心卖点还没开建。
- **活体冒烟与 pty 手动冒烟**未逐项留痕；TUI 胶水层（Bubble Tea model、glamour、bridge）无自动化测试覆盖，诚实标注。

## 下一步重点

两条线并行推进：
- 回到内核：**阶段 2 · A·横向爆破**。用 `errgroup` + `context` 取消，派 N 个 agent 竞赛解同一问题、取最优、败者退钱。开放问题是「裁判怎么判」和「一千个 agent 的限流/预算」。详见 [`ROADMAP.md`](ROADMAP.md)。
- **IM 通道（飞书优先）**：`internal/channel` 抽象 + 飞书长连接 adapter（免公网 IP），Event 地基已就位。
