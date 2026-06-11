package channel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "team.json"))
}

func pending(code, ws string, ttl time.Duration) Pending {
	return Pending{Code: code, Workspace: ws, ExpiresAt: time.Now().Add(ttl)}
}

func TestPairFlow(t *testing.T) {
	s := tempStore(t)
	if err := s.AddPending(Pending{
		Code: "ABCD2345", Name: "小明", Workspace: "/ws/a",
		AllowedTools: []string{"read", "bash"}, Model: "k2",
		ExpiresAt: time.Now().Add(PairTTL),
	}); err != nil {
		t.Fatalf("AddPending: %v", err)
	}

	// 小写带空白也能认领（归一化）。
	b, ok, err := s.Pair("feishu", "ou_1", "fallback", " abcd2345 ")
	if err != nil || !ok {
		t.Fatalf("Pair: ok=%v err=%v", ok, err)
	}
	if b.Workspace != "/ws/a" || b.Name != "小明" || b.Model != "k2" || len(b.AllowedTools) != 2 {
		t.Fatalf("绑定内容不对: %+v", b)
	}

	// 配对码单次有效。
	if _, ok, _ := s.Pair("feishu", "ou_2", "", "ABCD2345"); ok {
		t.Fatal("同一配对码被认领了两次")
	}

	// Find 命中。
	got, ok, err := s.Find("feishu", "ou_1")
	if err != nil || !ok || got.Workspace != "/ws/a" {
		t.Fatalf("Find: ok=%v err=%v got=%+v", ok, err, got)
	}
	// 不同通道不命中。
	if _, ok, _ := s.Find("dingtalk", "ou_1"); ok {
		t.Fatal("跨通道不应命中")
	}

	// 文件权限 0600。
	st, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("team.json 权限 = %v，要 0600", st.Mode().Perm())
	}
}

func TestPairNameFallback(t *testing.T) {
	s := tempStore(t)
	_ = s.AddPending(pending("AAAA2222", "/ws", PairTTL))
	b, ok, _ := s.Pair("feishu", "u", "IM显示名", "AAAA2222")
	if !ok || b.Name != "IM显示名" {
		t.Fatalf("pending 没备注名时应回落 IM 名: %+v", b)
	}
}

func TestPendingExpiry(t *testing.T) {
	s := tempStore(t)
	_ = s.AddPending(pending("EXPIRED2", "/ws", -time.Minute))
	if _, ok, _ := s.Pair("feishu", "u", "", "EXPIRED2"); ok {
		t.Fatal("过期码不应认领成功")
	}
}

func TestPendingLimit(t *testing.T) {
	s := tempStore(t)
	for _, c := range []string{"AAAA2345", "BBBB2345", "CCCC2345"} {
		if err := s.AddPending(pending(c, "/ws", PairTTL)); err != nil {
			t.Fatalf("AddPending %s: %v", c, err)
		}
	}
	if err := s.AddPending(pending("DDDD2345", "/ws", PairTTL)); err == nil {
		t.Fatal("第 4 个 pending 应报上限错误")
	}
	// 过期腾位后可以再加。
	s2 := tempStore(t)
	_ = s2.AddPending(pending("AAAA2345", "/ws", -time.Minute))
	_ = s2.AddPending(pending("BBBB2345", "/ws", PairTTL))
	_ = s2.AddPending(pending("CCCC2345", "/ws", PairTTL))
	if err := s2.AddPending(pending("DDDD2345", "/ws", PairTTL)); err != nil {
		t.Fatalf("过期码应被清理腾位: %v", err)
	}
}

func TestRebind(t *testing.T) {
	s := tempStore(t)
	_ = s.AddPending(pending("AAAA2222", "/ws/old", PairTTL))
	_, _, _ = s.Pair("feishu", "u", "", "AAAA2222")
	_ = s.AddPending(pending("BBBB3333", "/ws/new", PairTTL))
	if _, ok, _ := s.Pair("feishu", "u", "", "BBBB3333"); !ok {
		t.Fatal("换绑应成功")
	}
	team, _ := s.Load()
	if len(team.Bindings) != 1 || team.Bindings[0].Workspace != "/ws/new" {
		t.Fatalf("换绑应覆盖旧绑定: %+v", team.Bindings)
	}
}

func TestRemove(t *testing.T) {
	s := tempStore(t)
	_ = s.AddPending(pending("AAAA2222", "/ws", PairTTL))
	_, _, _ = s.Pair("feishu", "u", "", "AAAA2222")
	ok, err := s.Remove("feishu", "u")
	if err != nil || !ok {
		t.Fatalf("Remove: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.Find("feishu", "u"); ok {
		t.Fatal("删除后不应命中")
	}
	if ok, _ := s.Remove("feishu", "u"); ok {
		t.Fatal("重复删除应返回 false")
	}
}

func TestLoadMissingFile(t *testing.T) {
	s := tempStore(t)
	team, err := s.Load()
	if err != nil || len(team.Bindings) != 0 || len(team.Pending) != 0 {
		t.Fatalf("文件不存在应返回零值: %+v err=%v", team, err)
	}
}

func TestGenCodeAndLooksLike(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		c, err := GenCode()
		if err != nil {
			t.Fatalf("GenCode: %v", err)
		}
		if len(c) != codeLen {
			t.Fatalf("码长 = %d", len(c))
		}
		for _, r := range c {
			if !strings.ContainsRune(codeCharset, r) {
				t.Fatalf("码 %q 含字符集外字符", c)
			}
		}
		seen[c] = true
	}
	if len(seen) < 49 {
		t.Fatalf("50 个码出现明显重复（%d 个唯一），随机性可疑", len(seen))
	}

	if !LooksLikeCode(" abcd2345 ") {
		t.Fatal("归一后合法的码应判真")
	}
	for _, bad := range []string{"", "短", "ABCD234", "ABCD23456", "ABCD234!", "帮我看下代码", "ABCD2340"} {
		if LooksLikeCode(bad) {
			t.Fatalf("%q 不该像配对码", bad)
		}
	}
}
