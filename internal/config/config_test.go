package config

import (
	"os"
	"path/filepath"
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

// TestResolveProviderSyntax 验证 ② provider/model-id 语法。
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
	if tgt.Protocol != ProtocolOpenAIChat || tgt.BaseURL != "http://localhost:11434/v1" ||
		tgt.Model != "llama3" || tgt.APIKey != "" {
		t.Fatalf("provider target wrong: %+v", tgt)
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
