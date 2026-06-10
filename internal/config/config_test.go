package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig 在临时目录写一份假 config 并把 XDG_CONFIG_HOME 指过去。
func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "tokencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokencode", "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fixture = `{
	"providers": {
		"deepseek": {
			"base_url": "https://api.deepseek.com/anthropic",
			"protocol": "anthropic",
			"api_key_env": "TEST_DS_KEY",
			"auth": "bearer"
		},
		"ollama": {
			"base_url": "http://localhost:11434/v1",
			"protocol": "openai-chat"
		},
		"gemini": {
			"protocol": "google",
			"api_key_env": "TEST_GEMINI_KEY"
		}
	},
	"models": {
		"ds": "deepseek/deepseek-v4-pro[1m]",
		"local": "ollama/qwen3"
	},
	"default_model": "ds"
}`

// TestLoadNoFile 验证文件不存在时返回零值 config 而非错误，
// 且 Resolve 走原样直传（与无 config 时代行为一致）。
func TestLoadNoFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // 空目录，无 config.json

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultModel != "" || len(cfg.Providers) != 0 {
		t.Fatalf("expected zero config, got %+v", cfg)
	}

	tgt, err := cfg.Resolve("deepseek-v4-pro[1m]")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !tgt.Default || tgt.Model != "deepseek-v4-pro[1m]" || tgt.Protocol != ProtocolAnthropic {
		t.Fatalf("passthrough target wrong: %+v", tgt)
	}
}

// TestResolveAlias 验证 ① 别名路径，含 api_key_env 解析。
func TestResolveAlias(t *testing.T) {
	writeConfig(t, fixture)
	t.Setenv("TEST_DS_KEY", "sk-from-env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultModel != "ds" {
		t.Fatalf("default_model wrong: %q", cfg.DefaultModel)
	}

	tgt, err := cfg.Resolve("ds")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.Default {
		t.Fatal("alias should not be passthrough")
	}
	if tgt.Protocol != ProtocolAnthropic || tgt.BaseURL != "https://api.deepseek.com/anthropic" ||
		tgt.Model != "deepseek-v4-pro[1m]" || tgt.APIKey != "sk-from-env" || !tgt.Bearer {
		t.Fatalf("alias target wrong: %+v", tgt)
	}
}

// TestResolveProviderSyntax 验证 ② provider/model-id 语法，
// 且旧协议名 "openai-chat" 被归一化为 "openai"。
func TestResolveProviderSyntax(t *testing.T) {
	writeConfig(t, fixture)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	tgt, err := cfg.Resolve("ollama/llama3")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.Default {
		t.Fatal("provider syntax should not be passthrough")
	}
	if tgt.Protocol != ProtocolOpenAI || tgt.BaseURL != "http://localhost:11434/v1" ||
		tgt.Model != "llama3" || tgt.APIKey != "" {
		t.Fatalf("provider target wrong: %+v", tgt)
	}
}

// TestResolveGoogle 验证 google 协议 provider，base_url 缺省（由调用方落 Gemini 官方端点）。
func TestResolveGoogle(t *testing.T) {
	writeConfig(t, fixture)
	t.Setenv("TEST_GEMINI_KEY", "g-from-env")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	tgt, err := cfg.Resolve("gemini/gemini-2.5-pro")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tgt.Protocol != ProtocolGoogle || tgt.BaseURL != "" ||
		tgt.Model != "gemini-2.5-pro" || tgt.APIKey != "g-from-env" {
		t.Fatalf("google target wrong: %+v", tgt)
	}
}

// TestResolveBadProtocol 验证未知协议名报错。
func TestResolveBadProtocol(t *testing.T) {
	writeConfig(t, `{"providers": {"x": {"base_url": "http://x", "protocol": "grpc"}}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.Resolve("x/model-1"); err == nil {
		t.Fatal("expected error for unknown protocol")
	}
}

// TestResolvePassthrough 验证 ③：别名、已知 provider 前缀都不中时原样直传，
// 含 model id 本身带 "/" 的情况（如 OpenRouter 风格 id）。
func TestResolvePassthrough(t *testing.T) {
	writeConfig(t, fixture)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, m := range []string{"deepseek-v4-pro[1m]", "unknown/some-model"} {
		tgt, err := cfg.Resolve(m)
		if err != nil {
			t.Fatalf("resolve %q: %v", m, err)
		}
		if !tgt.Default || tgt.Model != m {
			t.Fatalf("passthrough %q wrong: %+v", m, tgt)
		}
	}
}

// TestResolveCatalog 验证内置目录解析：env key 激活、缺 key 给指引、
// 别名也能指向目录条目、用户 config 同名 provider 压过目录。
func TestResolveCatalog(t *testing.T) {
	writeConfig(t, `{"models": {"k2alias": "kimi-for-coding/kimi-test-model"}}`)
	t.Setenv("KIMI_API_KEY", "") // 开发机可能真设了这个变量，先清掉
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// 无凭据：报错并指引 auth login。
	if _, err := cfg.Resolve("kimi-for-coding/kimi-test-model"); err == nil ||
		!strings.Contains(err.Error(), "auth login kimi-for-coding") {
		t.Fatalf("missing key should hint auth login, got %v", err)
	}

	// env key 激活：协议/端点来自目录。
	t.Setenv("KIMI_API_KEY", "sk-kimi")
	tgt, err := cfg.Resolve("kimi-for-coding/kimi-test-model")
	if err != nil {
		t.Fatalf("resolve via catalog: %v", err)
	}
	if tgt.Protocol != ProtocolAnthropic || tgt.APIKey != "sk-kimi" || !tgt.Bearer ||
		!strings.Contains(tgt.BaseURL, "kimi.com") || tgt.Model != "kimi-test-model" {
		t.Fatalf("catalog target wrong: %+v", tgt)
	}

	// 别名指向目录条目。
	if tgt, err = cfg.Resolve("k2alias"); err != nil || tgt.APIKey != "sk-kimi" {
		t.Fatalf("alias via catalog: %+v, %v", tgt, err)
	}

	// 用户 config 同名 provider 压过目录。
	writeConfig(t, `{"providers": {"kimi-for-coding": {"base_url": "https://my.proxy/v1", "protocol": "openai", "api_key": "sk-mine"}}}`)
	cfg, _ = Load()
	tgt, err = cfg.Resolve("kimi-for-coding/m")
	if err != nil || tgt.BaseURL != "https://my.proxy/v1" || tgt.Protocol != ProtocolOpenAI {
		t.Fatalf("user config should shadow catalog: %+v, %v", tgt, err)
	}
}

// TestResolveBadAlias 验证别名指向未知 provider 时报错。
func TestResolveBadAlias(t *testing.T) {
	writeConfig(t, `{"models": {"bad": "nowhere/model-x"}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.Resolve("bad"); err == nil {
		t.Fatal("expected error for alias pointing to unknown provider")
	}
}
