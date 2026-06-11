package config

import "os"

// TokenCode 的环境变量体系：自有 TOKENCODE_* 优先，同名语义的
// ANTHROPIC_*（Claude Code 惯例）兜底——CC 用户的环境零改动可迁移。
//
//	TOKENCODE_MODEL       > ANTHROPIC_MODEL        默认模型
//	TOKENCODE_BASE_URL    > ANTHROPIC_BASE_URL     默认端点（anthropic 协议）
//	TOKENCODE_AUTH_TOKEN  > ANTHROPIC_AUTH_TOKEN   Bearer 凭据（优先）
//	TOKENCODE_API_KEY     > ANTHROPIC_API_KEY      x-api-key 凭据
//
// 其余 TOKENCODE_*（THEME/CATALOG/PULSE_LOG…）各自就近文档化。

// EnvFirst 返回第一个非空的环境变量值（全空返回 ""）。
func EnvFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// EnvModel 返回环境变量指定的默认模型（空=未指定）。
func EnvModel() string {
	return EnvFirst("TOKENCODE_MODEL", "ANTHROPIC_MODEL")
}

// EnvBaseURL 返回环境变量指定的默认端点（空=未指定）。
func EnvBaseURL() string {
	return EnvFirst("TOKENCODE_BASE_URL", "ANTHROPIC_BASE_URL")
}

// EnvAuth 返回环境变量里的默认 provider 凭据。bearer 表示走
// Authorization: Bearer（auth token 语义），否则 x-api-key。
// 同一语义内 TOKENCODE 前缀压过 ANTHROPIC 前缀。
func EnvAuth() (key string, bearer bool, ok bool) {
	if t := EnvFirst("TOKENCODE_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"); t != "" {
		return t, true, true
	}
	if k := EnvFirst("TOKENCODE_API_KEY", "ANTHROPIC_API_KEY"); k != "" {
		return k, false, true
	}
	return "", false, false
}
