package pulse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yzfly/tokencode/internal/llm"
)

// fakeLLM 返回固定文本并记录最后一次请求。
type fakeLLM struct {
	text    string
	calls   int
	lastReq llm.Request
}

func (f *fakeLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.lastReq = req
	f.calls++
	return llm.Response{Text: f.text, StopReason: llm.StopEndTurn}, nil
}

func someHistory(n int) []llm.Message {
	out := make([]llm.Message, n)
	for i := range out {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		out[i] = llm.Message{Role: role, Text: "消息内容"}
	}
	return out
}

func TestDreamerDueBoundaries(t *testing.T) {
	now := time.Now()
	d := NewDreamer(DreamConfig{}, nil) // 默认：AfterIdle 10m, MinNewMsgs 8, MinInterval 1h, MaxPerDay 6

	if d.Due(5*time.Minute, 20, now) {
		t.Fatal("空闲不足不应做梦")
	}
	if d.Due(time.Hour, 3, now) {
		t.Fatal("新材料不足不应做梦")
	}
	if !d.Due(time.Hour, 20, now) {
		t.Fatal("空闲够、有料、无历史梦时应做梦")
	}

	// 间隔不足。
	d.mu.Lock()
	d.last = now.Add(-30 * time.Minute)
	d.day = now
	d.mu.Unlock()
	if d.Due(time.Hour, 20, now) {
		t.Fatal("距上个梦不足 MinInterval 不应做梦")
	}

	// 超每日上限。
	d.mu.Lock()
	d.last = now.Add(-2 * time.Hour)
	d.day = now
	d.today = 6
	d.mu.Unlock()
	if d.Due(time.Hour, 20, now) {
		t.Fatal("超每日上限不应做梦")
	}
	// 跨天后计数重置。
	if !d.Due(time.Hour, 20, now.Add(24*time.Hour)) {
		t.Fatal("跨天后计数应重置")
	}
}

func TestDreamWritesAndRewritesMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tokencode", "memory.md")
	fake := &fakeLLM{text: "- 用户偏好 Go\n- 项目用 SQLite"}
	d := NewDreamer(DreamConfig{Model: "m", MemoryPath: path}, fake)

	if err := d.Dream(context.Background(), someHistory(10)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "用户偏好 Go") {
		t.Fatalf("memory.md 内容错误：%q", b)
	}
	// 喂给梦的 prompt 应带旧记忆与对话。
	if !strings.Contains(fake.lastReq.Messages[0].Text, "（空）") {
		t.Fatal("首梦的旧记忆应标记为空")
	}

	// 重写而非追加：第二个梦后旧内容消失。
	fake.text = "- 只剩这一条"
	if err := d.Dream(context.Background(), someHistory(10)); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "用户偏好 Go") || !strings.Contains(string(b), "只剩这一条") {
		t.Fatalf("memory.md 应被整体重写：%q", b)
	}
	// 第二个梦应读到旧记忆。
	if !strings.Contains(fake.lastReq.Messages[0].Text, "用户偏好 Go") {
		t.Fatal("旧记忆应喂给下一个梦")
	}
	// seen 更新：同样长度的历史不再算新材料。
	if d.seen() != 10 {
		t.Fatalf("梦成后 seenLen 应为 10，得到 %d", d.seen())
	}

	// 原子写不留临时文件。
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("不应残留临时文件：%s", e.Name())
		}
	}
}

func TestDreamSingleFlight(t *testing.T) {
	fake := &fakeLLM{text: "x"}
	d := NewDreamer(DreamConfig{Model: "m", MemoryPath: filepath.Join(t.TempDir(), "memory.md")}, fake)
	d.sem <- struct{}{} // 模拟已有梦在做
	if err := d.Dream(context.Background(), someHistory(10)); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 0 {
		t.Fatal("已有梦在做时不应再调 LLM")
	}
}
