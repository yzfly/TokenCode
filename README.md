# TokenCode

> **为并行而生的 Agent 运行时。** 当前阶段：一个极简的 Go 终端编码 agent，内置协议转换层、可接入任意模型，作为后续并行运行时的底座。路线见 [`ROADMAP.md`](ROADMAP.md)。

![TokenCode TUI 截图](assets/screenshot.png)

## 它是什么

一个跑在终端里的编码 agent：你说一句话，它用 `read` / `write` / `edit` / `bash` 四个工具读改文件、跑命令，循环直到把活干完。流式输出、会话自动落盘（`-continue` 随时接着聊）。核心是标准的 tool-use 循环：

```
用户消息 → LLM(带工具) → tool_use → 执行 → tool_result 回灌 → 循环 → 结束
```

- **走 Anthropic 协议**，默认通过 [DeepSeek](https://platform.deepseek.com/) 的 Anthropic 兼容端点接入，默认模型 `deepseek-v4-pro[1m]`。换 base URL 即可指向官方 Anthropic 或任何兼容服务。
- **零第三方依赖**，纯 Go 标准库，编出单个静态二进制。

## 快速开始

```bash
export ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
export ANTHROPIC_AUTH_TOKEN=<你的 DeepSeek API Key>
export ANTHROPIC_MODEL=deepseek-v4-pro[1m]

go run ./cmd/tokencode
# 或编译： go build -o bin/tokencode ./cmd/tokencode && ./bin/tokencode
```

进入后直接输入指令，例如 `create hello.txt containing hi, then read it back`。

权限三模式：**plan**（只读）/ **review**（逐次 y/n/a 确认，默认）/ **yolo**（全放行）。
Shift+Tab 循环切换，或用 `/plan` `/review` `/yolo`；`/exit` 或 Ctrl-D 退出，跑动中 Ctrl-C 打断当前轮。

输入 `/` 弹出命令补全菜单：`/help` 全部命令与快捷键、`/model` 查看与热切换模型、`/skills` 技能列表（`/技能名 [参数]` 调用，兼容 `.claude/skills`）、`/mcp` MCP server 状态与重连。

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

### 接入其它模型

不写 config 时行为与上面完全一致（ANTHROPIC_* 环境变量 + DeepSeek 端点）。要接入更多模型，在 `~/.config/tokencode/config.json`（或 `$XDG_CONFIG_HOME/tokencode/config.json`）注册 provider：

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

- [x] 单 agent tool-use 循环（本 MVP）
- [x] streaming、会话持久化（`-continue`/`-resume`）、多 provider（anthropic/openai/google 三协议）
- [ ] A · 横向爆破（竞赛）：派 N 个、取最优、败者退钱
- [ ] 工作区权威（单写者）+ 三方合并：甲模式的并行写

## 参与 / 了解进展

TokenCode 想做成一个「一起来做 Agent」的开源项目——除了代码，每天的开发过程、当前状态和整张路线图都公开，供后来者学习或接力：

- [`devlog/`](devlog/) —— 开发日记，按天记录「今天做了什么 / 是什么状态 / 为什么这么选」。
- [`ROADMAP.md`](ROADMAP.md) —— 从内核推导的整张路线图，分阶段并标注当前状态。
- [`STATUS.md`](STATUS.md) —— Agent 当前状态快照，5 分钟看懂「现在是什么、到哪一步了」。

## 作者与许可

- 作者：云中江树（微信公众号：云中江树）
- 许可：[CC BY-NC 4.0](LICENSE)（非商用）
