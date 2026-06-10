package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptInjectsMemory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if strings.Contains(SystemPrompt(), "长期记忆") {
		t.Fatal("无 memory.md 时不应有长期记忆节")
	}

	if err := os.MkdirAll(filepath.Dir(MemoryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MemoryPath, []byte("- 用户偏好 Go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := SystemPrompt()
	if !strings.Contains(s, "## 长期记忆") || !strings.Contains(s, "用户偏好 Go") {
		t.Fatalf("memory.md 内容应注入 system prompt：\n%s", s)
	}

	// 空文件（仅空白）不注入。
	if err := os.WriteFile(MemoryPath, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(SystemPrompt(), "长期记忆") {
		t.Fatal("空 memory.md 不应注入")
	}
}
