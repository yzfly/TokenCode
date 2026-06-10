// Package tui 是 TokenCode 的 Bubble Tea 终端外壳：inline 模式，
// 成型内容用 glamour 渲染一次后 Println 进原生 scrollback，活区只留
// 输入框/状态栏/spinner——以此避开 opencode 那种长消息逐 token 重渲染的卡。
package tui

import (
	"bytes"
	"encoding/json"
	"strings"
)

// confirmChoice 是单键确认的解读结果。
type confirmChoice int

const (
	choiceReject      confirmChoice = iota // 拒绝本次工具调用
	choiceAllowOnce                        // 放行这一次
	choiceAllowAlways                      // 本会话内放行该工具
)

// interpretConfirmKey 把一个按键字节解读为确认选择。
// y=放行一次，a=本会话放行该工具，其余一律拒绝。
func interpretConfirmKey(b byte) confirmChoice {
	switch b {
	case 'y', 'Y':
		return choiceAllowOnce
	case 'a', 'A':
		return choiceAllowAlways
	default:
		return choiceReject
	}
}

// compactJSON 把 JSON 压成单行；非法 JSON 原样返回。
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// oneLine 把任意空白（换行/制表/连续空格）压成单个空格，再按 rune 截断
// （UTF-8 安全）。压制表符是关键：read 结果是带行号的 cat -n 文本，
// 不压的话 tab 会在终端按制表位撑成大片空白，整行糊掉。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
