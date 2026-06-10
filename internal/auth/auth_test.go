package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got := Get("kimi-for-coding"); got != "" {
		t.Errorf("empty store should yield no key, got %q", got)
	}
	if err := Set("kimi-for-coding", Credential{Type: "api", Key: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	if err := Set("deepseek", Credential{Type: "api", Key: "sk-ds"}); err != nil {
		t.Fatal(err)
	}
	if got := Get("kimi-for-coding"); got != "sk-test" {
		t.Errorf("Get = %q", got)
	}
	if ps := Providers(); len(ps) != 2 || ps[0] != "deepseek" {
		t.Errorf("Providers = %v", ps)
	}

	// 文件权限必须是 0600。
	st, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("auth.json perm = %v, want 0600", st.Mode().Perm())
	}

	if err := Remove("kimi-for-coding"); err != nil {
		t.Fatal(err)
	}
	if Get("kimi-for-coding") != "" {
		t.Error("removed credential should be gone")
	}
	if err := Remove("never-existed"); err != nil {
		t.Errorf("removing absent provider should be a no-op: %v", err)
	}
}

func TestPathFollowsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	want := filepath.Join(dir, "tokencode", "auth.json")
	if Path() != want {
		t.Errorf("Path = %s, want %s", Path(), want)
	}
}
