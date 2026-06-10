package catalog

import (
	"strings"
	"testing"
)

func TestEmbeddedCatalog(t *testing.T) {
	all := All()
	if len(all) < 100 {
		t.Fatalf("catalog too small: %d providers", len(all))
	}

	// 国内 coding plan 关键条目必须在且协议映射正确。
	cases := []struct {
		id, protocol string
		wantAPI      bool
	}{
		{"kimi-for-coding", "anthropic", true},
		{"zhipuai-coding-plan", "openai", true},
		{"minimax-cn-coding-plan", "anthropic", true},
		{"deepseek", "openai", true},
		{"tencent-coding-plan", "openai", true},
	}
	for _, c := range cases {
		p, ok := Find(c.id)
		if !ok {
			t.Errorf("missing provider %s", c.id)
			continue
		}
		if p.Protocol != c.protocol {
			t.Errorf("%s protocol = %s, want %s", c.id, p.Protocol, c.protocol)
		}
		if c.wantAPI && p.BaseURL == "" {
			t.Errorf("%s missing base url", c.id)
		}
		if len(p.Models) == 0 {
			t.Errorf("%s has no models", c.id)
		}
		if !p.Usable() {
			t.Errorf("%s should be usable", c.id)
		}
	}
}

func TestAnthropicBaseURLNormalized(t *testing.T) {
	// anthropic 条目的尾部 /v1 必须剥掉（codec 自己补 /v1/messages）。
	for _, id := range []string{"kimi-for-coding", "minimax-cn-coding-plan"} {
		p, ok := Find(id)
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if strings.HasSuffix(p.BaseURL, "/v1") {
			t.Errorf("%s base url should have /v1 stripped: %s", id, p.BaseURL)
		}
	}
}

func TestProtocolFor(t *testing.T) {
	cases := map[string]string{
		"@ai-sdk/anthropic":            "anthropic",
		"@ai-sdk/google":               "google",
		"@ai-sdk/google-vertex":        "google",
		"@ai-sdk/openai":               "openai",
		"@ai-sdk/openai-compatible":    "openai",
		"@openrouter/ai-sdk-provider":  "openai",
		"":                             "openai",
		"some-unknown-future-provider": "openai",
	}
	for npm, want := range cases {
		if got := protocolFor(npm); got != want {
			t.Errorf("protocolFor(%q) = %s, want %s", npm, got, want)
		}
	}
}

func TestKeyFromEnv(t *testing.T) {
	p := Provider{Env: []string{"TC_TEST_KEY_A", "TC_TEST_KEY_B"}}
	if p.KeyFromEnv() != "" {
		t.Error("no env set should yield empty")
	}
	t.Setenv("TC_TEST_KEY_B", "k2")
	if p.KeyFromEnv() != "k2" {
		t.Error("should pick first non-empty env")
	}
}
