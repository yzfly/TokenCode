# TokenCode 当前状态

> 此刻这个 Agent 是什么、到哪一步了。5 分钟看懂版。
> 更新于 2026-06-11。深一步看：[`ROADMAP.md`](ROADMAP.md)（整张图）、[`devlog/`](devlog/)（每天脚印）。

## 一句话

一个跑在终端里的**编码 agent**：单 agent tool-use 循环底座 + Bubble Tea TUI，工具有 read/write/edit/bash + websearch/webfetch（联网搜索，免 key），可经 agent/workflow 工具委托子代理与脚本化编排。**并行内核已开建**：`/race <N> <任务>` 派最多 1000 个 agent 各占一个 git worktree 独立竞赛解题，裁判流水线择优，确认后落地（ROADMAP 阶段 2 · A 的 v1）。另有**多模型协议转换层**（anthropic / openai / google）、**SSE 流式**、**会话 JSONL 持久化**、**心跳 + 自动做梦**。整张架构图见 `docs/architecture.md`（本地，未入库）。

## 路线图位置

- 阶段 0 · 单 agent MVP 底座 — **已完成**
- 阶段 1 · 交互外壳 TUI — **已完成 / 打磨中**
- 阶段 2 · A·横向爆破（竞赛）— **v1 已落地 · 打磨中**
- 阶段 3 · B·协作 + 工作区权威 — 未开始

## 现在能做什么（能力清单）

- 自然语言驱动的 tool-use 循环：消息 → LLM(带工具) → `tool_use` → 执行 → `tool_result` 回灌 → 循环 → 结束。
- 内置工具：`read`（只读免确认）/ `write` / `edit` / `bash` + **联网**：`websearch`（DuckDuckGo → Mojeek 自动回退，免 key 免费）/ `webfetch`（取网页转纯文本）。
- **并行竞赛模式（`/race`）**：`/race <N> <任务>`（N≤1000）——每个 racer 在自己的 git worktree（独立分支）里独立解题，工具被 per-registry 根硬隔离在自己写空间；窗口化并发（`race.concurrency`，默认 8）；裁判流水线 = 客观粗筛（空 diff / `race.check` 命令淘汰，零 token）→ 并行 LLM 打分 → top-4 决赛；排行榜出来后 `/race apply` 确认应用冠军 diff（不自动 commit）、`/race discard` 放弃；冠军分支保留可追溯；Ctrl+C 全员停手并清理。
- **子代理与工作流**：agent 工具委托隔离上下文子代理（并行安全）、workflow 工具跑 goja JS 编排脚本；自定义代理从 `.tokencode/agents`（兼容 `.claude/agents`）发现。
- **多模型**：内置协议转换层，协议按协议命名——`anthropic` / `openai` / `google` 三种 codec（旧值 `openai-chat` 兼容归一化）；`~/.config/tokencode/config.json` 注册 provider（DeepSeek/Kimi/Qwen/OpenRouter/Ollama/Gemini……），`-model 别名` 或 `-model provider/model-id` 切换；无 config 时默认经 DeepSeek 接入（`deepseek-v4-pro[1m]`），行为与之前一致。
- **模型目录与 coding plan 开箱即用**：embed 一份 [models.dev](https://models.dev) 裁剪快照（141 个 provider），Kimi for Coding / 智谱 GLM / 阿里百炼 / MiniMax / 腾讯混元等国内 coding plan 零配置可选；`tokencode models` 浏览、`tokencode auth login <provider>` 存 key（auth.json，0600）；解析顺序 config 别名 → config provider → 内置目录 → 直传。
- **上下文压缩与可视化（长会话生存能力）**：`/compact [侧重点]` 把除最近 2 个完整轮次外的历史交给当前模型生成结构化摘要（保留任务目标/关键决定/文件路径/未完成事项），替换为一条 `[历史压缩摘要]` user 消息；token 估算无 tokenizer 依赖（UTF-8 字节/3.5 + 消息与工具块常数开销，偏保守），超过 `compact.auto_threshold`（缺省 80000，显式 0 关闭）时 turn 开始前自动压缩并提示；`/context` 看估算 tokens、最近一次真实 input tokens、历史条数与各类占比、距阈值余量。v0 取舍：压缩只影响本进程内存——JSONL 落盘 append-only 不改写、摘要不落盘，`-continue`/`-resume` 恢复回未压缩的全量历史。
- **命令体系**：命令注册表驱动的 `/help /model /skills /mcp /usage /context /compact /plan /review /yolo /exit`；输入 `/` 弹补全菜单（前缀优先、↑↓ 选、Tab 补全、Enter 执行、Esc 关）；未知命令不发模型、给就近建议；`//` 转义发普通消息。`/model <名>` 运行时热切换模型。需求全图见 `docs/requirements/tui-commands.md`（P1/P2 排好了队）。
- **Skills（M3 lite）**：扫 `.tokencode/skills` 并兼容 `.claude/skills`、`.agents/skills`，启动只读 frontmatter，`/技能名 [参数]` 调用时才读正文（渐进披露）；$ARGUMENTS 替换对齐 Claude Code 语义。
- **MCP（最小 stdio client）**：config.json 的 `"mcp"` 字段配置 server，后台连接绝不阻塞启动；工具以 `mcp__server__tool` 注册进 registry，对 agent 与内置工具零差别；`/mcp` 看状态、`/mcp reconnect <名>` 重连；退出硬 kill 子进程。
- **流式输出**：三协议都实现 `llm.Streamer`（SSE），TUI 实时显示生成中的文本尾巴（完成后整段 markdown 渲染替换），plain 模式逐段打印；codec 不支持或外壳不要增量时自动回落非流式。
- **token 用量记账（WebUI 大盘的地基）**：每次模型调用（用户/子代理/racer/裁判/心跳/梦/headless/serve 全路径）把 in/out/cache 用量追加进 `$XDG_DATA_HOME/tokencode/usage/YYYY-MM.jsonl`（append-only、损坏行跳过、失败静默不影响对话）；三协议含流式都解析 usage（含缓存读写）；`/usage`（别名 `/cost` `/stats`）看本月/今日合计与按模型/来源排行，聚合经 `usage.Summarize` 供将来 WebUI 共用。
- **会话持久化**：每拍结束把新留下的历史追加进 `$XDG_DATA_HOME/tokencode/sessions/YYYY/MM/DD/<id>.jsonl`（append-only，崩溃安全，半行损坏自动跳过）；`-continue` 继续当前目录最近会话、`-resume <id>` 指定恢复、`-no-session` 关闭；心跳空转拍剔除后不落盘（水位线机制）。
- **Event 即拍**：用户消息、心跳、梦醒是同一个 `Event` 原语的不同来源，agent 是消费 `chan Event` 的 actor，所有 turn 串行（单写者）。
- **心跳**（`-heartbeat 30m` 显式开启）：三级短路省 token——L0 本地检查零 token → L1 空转回哨兵 `HEARTBEAT_OK` 即从历史剔除 → L2 真有事才完整跑；非交互 turn 只读放行、写类拒绝。
- **自动做梦**：空闲 ∧ 有料双条件触发，独立 goroutine 调一次 LLM 把会话压缩成 `.tokencode/memory.md`，system prompt 注入「长期记忆」——重启也记得。
- Bubble Tea TUI（alt-screen + viewport）：glamour markdown 每条渲染一次进缓存、DeepSeek 蓝主题、亮/暗自适应、鼠标滚轮滚动、字符 logo + 欢迎卡、等待时 spinner。
- 权限四模式：**plan**（只读）/ **review**（逐次 y/n/a 确认，默认）/ **auto**（小模型按规则裁决）/ **yolo**（全放行）；Shift+Tab 循环或 `/plan` `/review` `/auto` `/yolo` 切换。
- **权限规则三表（CC 语法，团队治理地基）**：config.json 的 `permissions` + 项目级 `.tokencode/permissions.json`（合并取并集）声明 `allow`/`ask`/`deny`——`read`、`bash(git log *)`、`agent(explore)`、`mcp__server__*` 这类规则；bash 复合命令拆段逐判（任一段 deny 即 deny、全段 allow 才 allow、命令替换段永不 allow）。优先级 deny > plan 只读铁律 > ask（yolo/auto 也强制人工确认）> allow > 模式默认；deny 在 TUI/headless/serve/IM 通道全入口生效。与 `.tokencode/permissions.md`（auto 裁决器的自然语言软提示）并存分工。
- 跑动中 Ctrl-C 打断当前轮、回提示符；非 tty（管道/重定向）自动退化为纯文本。
- **Headless 与 HTTP API（SDK/通道/WebUI 的共同地基）**：`-p` 跑一个 turn 即退出（`-output text|json|stream-json`，stream-json 是 JSONL 事件流、最后一行恒为 result；成功 0 / 出错 1）；`tokencode serve` 起 HTTP API（`GET /healthz` + `POST /v1/run`，同步 JSON 或 SSE 流，默认仅回环、`-max-concurrent` 信号量限流、每请求独立 agent）。两条入口共用 `internal/headless` 的执行/事件/白名单语义：守卫包在工具层（白名单外喂回 "rejected (headless)"），子代理经共享注册表自动继承。
- **团队模式 · IM 通道（飞书 v1）**：成员用自己的飞书账号远程驱动自己的工作空间——`internal/channel` 通道抽象（Adapter/Router/team store）+ 飞书长连接 adapter（官方 SDK ws，免公网 IP，3 秒 ack 红线内异步投递、event_id 去重）。`tokencode team pair -workspace <目录>` 生成 8 位配对码（1 小时/单次有效，最多 3 个 pending，存 `team.json` 0600 原子写），成员在 IM 单聊发码即绑定；绑定可配工具白名单（默认只读集）/ yolo / 专属模型。每个 `channel+user+chat` 一个常驻 agent（内存历史、TryLock 互斥，在跑时新消息回「稍候」），turn 开始回「收到」、结束回最终文本；工具被 SetRoot 硬隔离在成员 workspace 内，用量记账 Source=`channel:<名>`。钉钉 Stream adapter 同构落地（官方 SDK 长连接收机器人单聊、msgId 去重、sessionWebhook 回文本），与飞书共用 Router 与配对流程。v0 边界：只处理单聊文本，卡片流式/审批按钮/群聊/企微后置。
- **团队模式 · IM 通道（飞书 v1）**：成员用自己的飞书账号远程驱动自己的工作空间——`internal/channel` 通道抽象（Adapter/Router/team store）+ 飞书长连接 adapter（官方 SDK ws，免公网 IP，3 秒 ack 红线内异步投递、event_id 去重）。`tokencode team pair -workspace <目录>` 生成 8 位配对码（1 小时/单次有效，最多 3 个 pending，存 `team.json` 0600 原子写），成员在 IM 单聊发码即绑定；绑定可配工具白名单（默认只读集）/ yolo / 专属模型。每个 `channel+user+chat` 一个常驻 agent（内存历史、TryLock 互斥，在跑时新消息回「稍候」），turn 开始回「收到」、结束回最终文本；工具被 SetRoot 硬隔离在成员 workspace 内，用量记账 Source=`channel:feishu`。v0 边界：只处理单聊文本，卡片流式/审批按钮/群聊/钉钉企微后置。
- **微信通道（iLink Bot API，实验性）**：`tokencode wechat login` 扫码接入（独立 bot 身份、DM-only），`internal/channel/wechat` 纯 Go 实现协议客户端 + adapter（getupdates 长轮询、context_token 按 peer 持久化、游标落盘续传、msg_id+内容 MD5 双重去重、-2 限频退避、-14 过期暂停提示重扫），凭证/状态存 `~/.config/tokencode/wechat/`（0600）；config `channels.wechat.enabled` 显式开关，协议灰度期 `base_url` 可覆盖。

## 架构概览（各包职责）

```
cmd/tokencode/main.go      解析 flag/env/config → 按协议构造 client → tui.Run
internal/llm/              LLM interface（统一 IR）+ anthropic / openai / google 三协议 codec
                           + Streamer 流式接口与共用 SSE 读取器
internal/config/           模型注册表 + MCP server 配置（JSON）+ 内置目录解析
internal/catalog/          embed 的 models.dev provider 目录（协议映射 / 端点 / key 探测）
internal/auth/             provider 凭据存储（auth.json，0600，`tokencode auth` 子命令）
internal/session/          会话 JSONL 持久化（Create/Open/Append/Load/Latest）
internal/usage/            token 用量记账（月度 JSONL + Log/Summarize，agent 循环统一拦截）
internal/skill/            Agent Skills 加载器（frontmatter 索引 + 正文懒加载）
internal/mcp/              MCP stdio client（JSON-RPC 握手/工具发现/调用 + Manager）
internal/agent/            Agent：对话状态 + Run/Serve（Event actor）+ Snapshot + 持久化水位线
internal/pulse/            心跳（Ticker + L0 短路）+ 做梦（Dreamer → memory.md）
internal/subagent/         子代理：类型发现 + Runner（Spawn/SpawnDef，工具子集 + 根隔离）
internal/workflow/         goja JS 编排脚本（agent/parallel/log 三原语）
internal/race/             并行竞赛：worktree 生命周期 + 窗口扇出 + 裁判流水线（零内部依赖，Spawn/Complete 注入）
internal/permrules/        权限规则三表（allow/ask/deny，CC 语法 glob 匹配 + bash 拆段，独立可测）
internal/headless/         无界面单 turn 执行：Run/Execute + 事件流 + 工具层白名单守卫（-p 与 serve 共用）
internal/serve/            HTTP API 雏形：/healthz + /v1/run（同步 JSON / SSE），装配经 Assemble 注入
internal/channel/          IM 通道体系：Adapter 抽象 + Router（绑定路由/配对/常驻会话）+ team store（team.json）
internal/channel/feishu/   飞书 adapter：官方 SDK 长连接收 im.message.receive_v1、im/v1/messages 发文本
internal/channel/dingtalk/ 钉钉 adapter：官方 Stream SDK 长连接收机器人单聊、sessionWebhook 发文本
internal/tools/            Tool interface + Registry（可绑定 per-agent 根）+ read/write/edit/bash/websearch/webfetch
internal/tui/              Bubble Tea 外壳（alt-screen + viewport）
  run.go      启动 program + Serve/Pulse goroutine + 非 tty 回退
  model.go    Bubble Tea Model：textarea + spinner + 状态栏 + 历史 + 确认
  messages.go msg 类型 + bridge（agent.UI 回调 → program.Send）
  theme.go    DeepSeek 配色（lipgloss + glamour，AdaptiveColor 亮暗两套）
  markdown.go renderMarkdown(md, width)
  commands.go 命令注册表：/help、/ 补全菜单、分发三处同源（+ commands_test.go）
  perms.go    加锁的权限状态 + decide() 裁决 + resolveGate() 规则/模式合成（+ perms_test.go）
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

常用 flag：`-model`、`-base-url`、`-max-tokens`（默认 4096）、`-yolo`（初始 yolo 模式）、`-theme auto|light|dark`、`-heartbeat 30m`（心跳，默认关闭）、`-continue`（继续最近会话）、`-resume <id>`、`-no-session`（不落盘）、`-p`（headless 单 turn）、`-output text|json|stream-json`、`-allowed-tools`（headless 白名单）。

**依赖**：阶段 0 是零第三方依赖；阶段 1 引入了 charmbracelet 那套（bubbletea / bubbles / glamour / lipgloss）+ `golang.org/x/term` 及一长串间接依赖。`go.mod` 现在有 require。这是为 TUI 观感有意识付的账。

## 当前已知限制

- **无终端原生 scrollback**：alt-screen 的代价，滚动靠 viewport + 滚轮；想找回原生滚动得换 inline 模式并解决其 resize 残留。
- **会话无分支/fork**：持久化是线性 append；TUI resume 后不回放历史画面（模型记得，屏幕不重绘旧消息）。
- **流式的 thinking 不上屏**：Delta 里有 Thinking 字段，外壳暂未展示。
- **做梦 v1 用主模型**：`DreamModel` 便宜档配置留位未接。
- **plain（非 tty）模式心跳不生效**：runPlain 同步调 Run，与 actor 并行会破坏单写者不变量；plain 模式也没有 `/race`。
- **race v1 的取舍**：racer 在自己 worktree 内**自动放行全部工具（含 bash）**——文件工具有根隔离硬约束，bash 只有 cwd 约束没有沙箱；全员跑完才裁判（无提前终止）；无预算/花费建模；裁判单评委（无投票）。
- **websearch 看引擎脸色**：DDG 对部分 IP 丢 202 反爬挑战（已自动回退 Mojeek）；解析靠正则，引擎改版需要跟进。
- **IM 通道 v0 的取舍**：会话历史驻 serve 进程内存（重启清零、不落 session JSONL）；换绑/改白名单对已创建的内存会话不生效（下一条消息仍用旧装配，重启后生效）；在跑时新消息直接回「稍候」不排队；进度只有「收到」一句，工具调用不逐条转发。
- **活体冒烟与 pty 手动冒烟**未逐项留痕；TUI 胶水层（Bubble Tea model、glamour、bridge）无自动化测试覆盖，诚实标注。

## 下一步重点

- **race 打磨**：败者提前退钱（够好即 cancel 全场）、多视角/投票裁判、预算上限、视角变体注入（variant 已留位）。
- **IM 通道扩展**：飞书/钉钉已落地；下一步卡片流式进度、权限审批按钮、群聊@、企微智能机器人。
