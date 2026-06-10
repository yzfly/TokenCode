package agent

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// SystemPrompt 构造精简系统提示（保持克制：只给最少的必要约束）。
// memory.md 存在且非空时附加「长期记忆」一节；文件不变时输出逐字节稳定，不破坏 prompt 缓存。
func SystemPrompt() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	base := fmt.Sprintf(`You are TokenCode, a coding agent working directly in the user's project.

Core tools: read, write, edit, bash. Use them to inspect files, change code, and run commands. Read a file before editing it. Prefer edit over write for small changes.

Delegation: the agent tool runs a sub-agent in an isolated context — use it for self-contained subtasks and broad searches whose details you don't need; launch several in one message to run them in parallel. The workflow tool orchestrates many sub-agents with a script — use it only for fan-out/aggregation over lists or repetitive multi-agent pipelines. For ordinary tasks just use the core tools directly.

Be concise. Do the work instead of narrating it. When the task is finished, give a one or two sentence summary.

Environment:
- Working directory: %s
- OS: %s`, cwd, runtime.GOOS)

	if mem := readMemory(MemoryPath); mem != "" {
		base += "\n\n## 长期记忆\n（来自 " + MemoryPath + "，空闲时自动整理）\n\n" + mem
	}
	return base
}

func readMemory(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
