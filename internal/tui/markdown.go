package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
)

// renderMarkdown 按给定宽度把 markdown 渲染成带 ANSI 样式的字符串。
// 在 Update 里调用（单线程、宽度已知），每条消息只渲染一次。
// 任何错误都退化为原文，绝不丢内容。
func renderMarkdown(md string, width int) string {
	if width <= 0 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(deepseekMarkdownStyle(uiDark)),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return strings.TrimRight(md, "\n")
	}
	out, err := r.Render(md)
	if err != nil {
		return strings.TrimRight(md, "\n")
	}
	return strings.TrimRight(out, "\n")
}
