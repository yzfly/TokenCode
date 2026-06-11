package checkpoint_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yzfly/tokencode/internal/checkpoint"
	"github.com/yzfly/tokencode/internal/tools"
)

// execTool 经注册表执行一次工具（走真实的 ctx 注入与拦截路径）。
func execTool(t *testing.T, reg *tools.Registry, name string, args map[string]any) {
	t.Helper()
	in, _ := json.Marshal(args)
	if _, err := reg.Execute(context.Background(), name, in); err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// TestFullChain 全链路：write 覆盖 + edit + 新建文件，分两个 turn，
// 逐级回滚验证内容恢复与新建文件删除。
func TestFullChain(t *testing.T) {
	work := t.TempDir()
	a := filepath.Join(work, "a.txt")
	b := filepath.Join(work, "b.txt")
	if err := os.WriteFile(a, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ck := checkpoint.New(filepath.Join(work, ".cp"))
	reg := tools.NewRegistry(tools.Write(), tools.Edit())
	reg.SetCheckpointer(ck.Snapshot)

	// turn 1：覆盖 a.txt v1→v2
	execTool(t, reg, "write", map[string]any{"path": a, "content": "v2"})
	// turn 2：edit a.txt v2→v3，并新建 b.txt
	ck.BeginTurn()
	execTool(t, reg, "edit", map[string]any{"path": a, "old_string": "v2", "new_string": "v3"})
	execTool(t, reg, "write", map[string]any{"path": b, "content": "new"})

	pts := ck.List()
	if len(pts) != 2 {
		t.Fatalf("应有 2 个检查点，得到 %d: %+v", len(pts), pts)
	}
	if pts[0].Files != 1 || pts[1].Files != 2 {
		t.Fatalf("文件数应为 1/2，得到 %d/%d", pts[0].Files, pts[1].Files)
	}

	// 回滚到 #2：撤销 turn 2 → a.txt 回到 v2，b.txt（当时不存在）删除。
	restored, err := ck.Rewind(2)
	if err != nil {
		t.Fatalf("Rewind(2): %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("应恢复 2 个文件，得到 %v", restored)
	}
	if got := readFile(t, a); got != "v2" {
		t.Fatalf("a.txt 应回到 v2，得到 %q", got)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("新建文件 b.txt 回滚后应被删除，stat err=%v", err)
	}

	// 连续回滚到 #1：a.txt 回到 v1。
	if _, err := ck.Rewind(1); err != nil {
		t.Fatalf("Rewind(1): %v", err)
	}
	if got := readFile(t, a); got != "v1" {
		t.Fatalf("a.txt 应回到 v1，得到 %q", got)
	}
	if len(ck.List()) != 0 {
		t.Fatalf("全部回滚后检查点应清空，得到 %+v", ck.List())
	}
}

// TestCorruptManifestSkipped manifest 里掺入损坏行：List/Rewind 跳过坏行照常工作。
func TestCorruptManifestSkipped(t *testing.T) {
	work := t.TempDir()
	a := filepath.Join(work, "a.txt")
	if err := os.WriteFile(a, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ck := checkpoint.New(filepath.Join(work, ".cp"))
	ck.Snapshot("write", a)
	if err := os.WriteFile(a, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 往 manifest 追加损坏行：非 JSON、空对象、半截 JSON。
	mf := filepath.Join(ck.Dir(), "manifest.jsonl")
	f, err := os.OpenFile(mf, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "这不是 JSON")
	fmt.Fprintln(f, "{}")
	fmt.Fprintln(f, `{"seq": 99, "turn":`)
	f.Close()

	pts := ck.List()
	if len(pts) != 1 || pts[0].Files != 1 {
		t.Fatalf("坏行应被跳过、只剩 1 个检查点，得到 %+v", pts)
	}
	if _, err := ck.Rewind(1); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if got := readFile(t, a); got != "v1" {
		t.Fatalf("a.txt 应回到 v1，得到 %q", got)
	}
}

// TestClearAndCleanOld /rewind clear 清空当次目录；CleanOld 只删超期目录。
func TestClearAndCleanOld(t *testing.T) {
	work := t.TempDir()
	base := filepath.Join(work, ".cp")
	a := filepath.Join(work, "a.txt")
	if err := os.WriteFile(a, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ck := checkpoint.New(base)
	ck.Snapshot("write", a)
	if err := ck.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(ck.List()) != 0 {
		t.Fatal("Clear 后应无检查点")
	}
	if _, err := os.Stat(ck.Dir()); !os.IsNotExist(err) {
		t.Fatal("Clear 后会话目录应被删除")
	}

	// CleanOld：0 龄阈值删掉刚建的目录，但不碰 base 下的普通文件。
	old := filepath.Join(base, "20200101-000000-1")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	checkpoint.CleanOld(base, 0)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("超期目录应被 CleanOld 删除")
	}
}
