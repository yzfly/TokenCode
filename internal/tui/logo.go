package tui

import (
	_ "embed"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// 两版字符 logo：宽终端用粗体 ANSI Shadow，窄终端用紧凑版。
//
//go:embed logo_big.txt
var logoBig string

//go:embed logo_small.txt
var logoSmall string

// renderBanner 拼出启动横幅：粗体蓝 logo + 标语 + DeepSeek 风格信息卡。
// width 来自首个 WindowSizeMsg，据此选 logo 与卡片宽度。
func renderBanner(modelName, baseURL string, mode permMode, width int) string {
	art := logoBig
	if width < 78 {
		art = logoSmall
	}
	logo := logoStyle.Render(strings.TrimRight(art, "\n"))
	tagline := taglineStyle.Render("Token 燃烧机，为 Token 燃烧而生")

	cardW := width - 4
	switch {
	case cardW > 72:
		cardW = 72
	case cardW < 32:
		cardW = 32
	}
	info := lipgloss.JoinVertical(lipgloss.Left,
		labelStyle.Render("模型")+"  "+valueStyle.Render(modelName)+"    "+labelStyle.Render("模式")+"  "+modeBadge(mode),
		labelStyle.Render("接入")+"  "+valueStyle.Render(baseURL),
		"",
		hintStyle.Render("⇧⇥ 切换模式 · /plan /review /yolo · ↑↓ 历史 · ^C 打断 · /exit 退出"),
	)
	card := cardStyle.Width(cardW).Render(info)

	return lipgloss.JoinVertical(lipgloss.Left, logo, "", tagline, "", card)
}
