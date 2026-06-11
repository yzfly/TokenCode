# TokenCode

> **为并行而生、对团队友好的 Agent 引擎。** 当前形态：一个 Go 终端编码 agent——单 agent 底座 + TUI 之上，已长出第一种并行模式 `/race`（最多 1000 个 agent 隔离竞赛解题、裁判择优）。方向：把编码 agent 从「个人终端工具」变成「团队基础设施」。路线见 [`ROADMAP.md`](ROADMAP.md)。

![TokenCode TUI 截图](assets/screenshot.png)

## 它是什么

一个跑在终端里的编码 agent：你说一句话，它用 `read` / `write` / `edit` / `bash` / `websearch` / `webfetch` 工具读改文件、跑命令、查资料，循环直到把活干完。流式输出、会话自动落盘（`-continue` 随时接着聊）、可委托子代理与 JS 工作流编排。核心是标准的 tool-use 循环：

```
用户消息 → LLM(带工具) → tool_use → 执行 → tool_result 回灌 → 循环 → 结束
```

- **走 Anthropic 协议**，默认通过 [DeepSeek](https://platform.deepseek.com/) 的 Anthropic 兼容端点接入，默认模型 `deepseek-v4-pro[1m]`。换 base URL 即可指向官方 Anthropic 或任何兼容服务。
- 单个静态二进制，开箱即用。

### `/race`：并行竞赛模式

```
/race 8 修复 internal/foo 的并发 bug，跑通全部测试
```

派 N 个 agent（N≤1000）**各自在隔离的 git worktree** 里独立解同一道题（文件工具被硬隔离在各自写空间），窗口化并发；跑完后裁判流水线择优——客观粗筛（空 diff / `race.check` 校验命令淘汰，零 token）→ 并行 LLM 打分 → top-4 决赛。排行榜出来后 `/race apply` 一键应用冠军改动（不自动 commit），冠军分支保留可追溯。

### 联网搜索

`websearch` 多后端自动回退：设置 `TAVILY_API_KEY` 时走 [Tavily](https://tavily.com)（LLM 友好摘录，免费档 1000 次/月），否则 DuckDuckGo → Mojeek 免 key 兜底——零配置可用，有配置更好。`webfetch` 抓网页转纯文本。

## 快速开始

```bash
export ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
export ANTHROPIC_AUTH_TOKEN=<你的 DeepSeek API Key>
export ANTHROPIC_MODEL=deepseek-v4-pro[1m]

go run ./cmd/tokencode
# 或编译： go build -o bin/tokencode ./cmd/tokencode && ./bin/tokencode
```

进入后直接输入指令，例如 `create hello.txt containing hi, then read it back`。

权限四模式：**plan**（只读）/ **review**（逐次 y/n/a 确认，默认）/ **auto**（小模型按规则自动裁决）/ **yolo**（全放行）。
Shift+Tab 循环切换，或用 `/plan` `/review` `/auto` `/yolo`；`/exit` 或 Ctrl-D 退出，跑动中 Ctrl-C 打断当前轮。

输入 `/` 弹出命令补全菜单：`/help` 全部命令与快捷键、`/race` 并行竞赛、`/model` 查看与热切换模型、`/agents` 子代理类型（兼容 `.claude/agents`）、`/skills` 技能列表（`/技能名 [参数]` 调用，兼容 `.claude/skills`）、`/mcp` MCP server 状态与重连。`! <命令>` 直通 shell。

### 常用 flag

| flag | 默认 | 说明 |
|------|------|------|
| `-model` | `$ANTHROPIC_MODEL` 或 `deepseek-v4-pro[1m]` | 模型 id |
| `-base-url` | `$ANTHROPIC_BASE_URL` 或 `https://api.deepseek.com/anthropic` | Anthropic 协议端点 |
| `-max-tokens` | `4096` | 单次最大输出 token |
| `-yolo` | `false` | 初始进入 yolo 模式（跳过写/改/执行确认） |
| `-theme` | `auto` | 配色主题：`auto` / `light` / `dark` |
| `-continue` | `false` | 继续当前目录最近一次会话 |
| `-resume` | — | 按会话 id 恢复 |
| `-no-session` | `false` | 本次会话不落盘 |
| `-p` | — | headless：跑一个 turn 后退出（`-p "任务"`，或管道 `echo 任务 \| tokencode -p`） |
| `-output` | `text` | headless 输出格式：`text` / `json` / `stream-json`（JSONL 事件流，仅 `-p` 下有效） |
| `-allowed-tools` | `read,websearch,webfetch` | headless 工具白名单（逗号分隔）；白名单外直接拒绝，`-yolo` 全放行 |

### Headless 与 HTTP API

无人值守的两种用法，权限语义相同（白名单外的工具调用直接拒绝、喂回模型）：

```bash
# headless：脚本/CI 里跑一个 turn 即退出（成功 0、出错 1）
tokencode -p "总结 README 的核心卖点" -output json
git diff | tokencode -p -allowed-tools read   # 管道喂 prompt

# HTTP API（v0 无鉴权，默认只绑回环）
tokencode serve -addr 127.0.0.1:8787
curl -s http://127.0.0.1:8787/v1/run -d '{"prompt":"列出当前目录结构","model":"可选"}'
# SSE 流式（事件与 -output stream-json 同构，最后一条恒为 result）
curl -N -H 'Accept: text/event-stream' http://127.0.0.1:8787/v1/run -d '{"prompt":"..."}'
```

每个请求独立 agent 实例（无共享历史），`-max-concurrent`（默认 8）限制同时在跑的 run。

### 模型与国内 Coding Plan 开箱即用

内置 [models.dev](https://models.dev) 目录快照（141 个 provider），**Kimi for Coding、智谱 GLM Coding Plan、阿里百炼 Coding Plan、MiniMax、DeepSeek、腾讯混元**等国内模型与包月套餐无需写任何配置：

```bash
tokencode models coding              # 浏览目录（按关键词过滤）
tokencode auth login kimi-for-coding # 粘贴 key，存入 auth.json（0600）
tokencode -model kimi-for-coding/k2p6
```

key 也可走环境变量（如 `KIMI_API_KEY`、`ZHIPU_API_KEY`，目录里每个条目都声明了探测变量）。目录快照用 `scripts/update-catalog.sh` 更新，`TOKENCODE_CATALOG` 可指向私有镜像。

### 手工注册 provider（config.json）

不写 config 时行为与上面完全一致（ANTHROPIC_* 环境变量 + DeepSeek 端点）。要精确控制端点/协议，在 `~/.config/tokencode/config.json`（或 `$XDG_CONFIG_HOME/tokencode/config.json`）注册 provider（同名条目压过内置目录）：

```json
{
  "providers": {
    "deepseek": {
      "base_url": "https://api.deepseek.com/anthropic",
      "protocol": "anthropic",
      "api_key_env": "DEEPSEEK_API_KEY",
      "auth": "bearer"
    },
    "kimi": {
      "base_url": "https://api.moonshot.cn/v1",
      "protocol": "openai",
      "api_key_env": "MOONSHOT_API_KEY"
    },
    "ollama": {
      "base_url": "http://localhost:11434/v1",
      "protocol": "openai"
    },
    "gemini": {
      "protocol": "google",
      "api_key_env": "GEMINI_API_KEY"
    }
  },
  "models": {
    "ds": "deepseek/deepseek-v4-pro[1m]",
    "local": "ollama/qwen3",
    "g": "gemini/gemini-2.5-pro"
  },
  "default_model": "ds"
}
```

- `protocol` 按协议命名，目前三种：`anthropic`、`openai`（Chat Completions，DeepSeek/Kimi/Qwen/OpenRouter/Ollama 通用，换 `base_url` 即可零代码接入）、`google`（Gemini，`base_url` 缺省指向官方端点）。旧值 `openai-chat` 仍兼容，等同 `openai`。
- key 推荐用 `api_key_env` 指向环境变量；本地 Ollama 不需要 key。
- 用法：`tokencode -model local`（别名）或 `tokencode -model ollama/llama3`（`provider/model-id`）。两者都不中时 `-model` 原样直传默认端点，兼容老用法。

## 开发

```bash
go test ./...     # 全部单测（工具层 / agent 循环 / LLM 协议层 httptest）
go vet ./...
```

## 路线

- [x] 单 agent tool-use 循环（MVP 底座）
- [x] streaming、会话持久化（`-continue`/`-resume`）、多 provider（anthropic/openai/google 三协议）
- [x] 子代理（agent 工具）+ 动态工作流（workflow JS 编排）+ 联网搜索（Tavily/DDG/Mojeek）
- [x] A · 横向爆破（竞赛）v1：`/race` 派 N 个、worktree 隔离、裁判择优 ——*打磨中：败者提前退钱、投票裁判、预算建模*
- [x] 模型与国内 coding plan 开箱即用（内置 models.dev 目录 + `tokencode auth login`）
- [ ] 团队接入：IM 通道体系（飞书/钉钉长连接 → 微信扫码/企微），每成员独立工作空间
- [ ] SDK/CLI 可编程化（headless `-p`、`serve`、Go SDK）与 WebUI 用量大盘
- [ ] 工作区权威（单写者）+ 三方合并：B · 协作模式的并行写

## 参与 / 了解进展

TokenCode 想做成一个「一起来做 Agent」的开源项目——除了代码，每天的开发过程、当前状态和整张路线图都公开，供后来者学习或接力：

- [`devlog/`](devlog/) —— 开发日记，按天记录「今天做了什么 / 是什么状态 / 为什么这么选」。
- [`ROADMAP.md`](ROADMAP.md) —— 从内核推导的整张路线图，分阶段并标注当前状态。
- [`STATUS.md`](STATUS.md) —— Agent 当前状态快照，5 分钟看懂「现在是什么、到哪一步了」。

## 作者与许可

- 作者：云中江树（微信公众号：云中江树）
- 许可：[CC BY-NC 4.0](LICENSE)（非商用）
