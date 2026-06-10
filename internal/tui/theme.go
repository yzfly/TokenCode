package tui

import (
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// deepSeekBlue 是 DeepSeek 界面强调色（社区公认值）。蓝在亮/暗底都清晰，做主强调色。
const deepSeekBlue = "#4D6BFE"

// uiDark 记录终端是否深色背景，在 tui.Run 启动时检测一次。
// markdown 渲染据此选亮/暗底；lipgloss 的 AdaptiveColor 也据此解析。
var uiDark bool

// 颜色：能两端通用的用单色，会随明暗反转的用 AdaptiveColor。
var (
	accent   = lipgloss.Color(deepSeekBlue)
	okColor  = lipgloss.AdaptiveColor{Light: "#1F9D55", Dark: "#3FB97A"}
	errColor = lipgloss.AdaptiveColor{Light: "#D63A3A", Dark: "#FF6B6B"}
	dimColor = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9AA0AC"}
	inkColor = lipgloss.AdaptiveColor{Light: "#444444", Dark: "#C8CCD4"}
)

var (
	// 输入框边框：focus 时蓝、blur 时暗灰。
	borderFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1)
	borderBlurred = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(dimColor).Padding(0, 1)

	statusStyle   = lipgloss.NewStyle().Foreground(dimColor)
	userStyle     = lipgloss.NewStyle().Foreground(accent).Bold(true)
	toolCallStyle = lipgloss.NewStyle().Foreground(accent)
	toolArgStyle  = lipgloss.NewStyle().Foreground(dimColor)
	okStyle       = lipgloss.NewStyle().Foreground(okColor)
	errStyle      = lipgloss.NewStyle().Foreground(errColor)
	spinnerStyle  = lipgloss.NewStyle().Foreground(accent)
	noteStyle     = lipgloss.NewStyle().Foreground(dimColor).Italic(true)
	hintStyle     = lipgloss.NewStyle().Foreground(dimColor)

	logoStyle    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	taglineStyle = lipgloss.NewStyle().Foreground(accent)

	// 信息卡：DeepSeek 风格——圆角边框、蓝标签、中性值。边框/值随明暗自适应。
	cardStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.AdaptiveColor{Light: "#BFC4CE", Dark: "#3D4350"}).Padding(0, 2)
	labelStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	valueStyle = lipgloss.NewStyle().Foreground(inkColor)

	// 模式徽标：plan 蓝、review 绿、auto 琥珀（半自动，提示留意）、yolo 红（醒目，提示有风险）。
	planBadge   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	reviewBadge = lipgloss.NewStyle().Foreground(okColor)
	autoBadge   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#F59E0B"}).Bold(true)
	yoloBadge   = lipgloss.NewStyle().Foreground(errColor).Bold(true)

	// 工具确认框：醒目——蓝色边框 + 反色徽标 + 高亮按键。
	confirmBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 1)
	confirmBadge    = lipgloss.NewStyle().Background(accent).Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	keyYes          = lipgloss.NewStyle().Foreground(okColor).Bold(true)
	keyAll          = lipgloss.NewStyle().Foreground(accent).Bold(true)
	keyNo           = lipgloss.NewStyle().Foreground(dimColor)

	// 折叠的历史工具执行提示。
	collapseStyle = lipgloss.NewStyle().Foreground(dimColor).Italic(true)

	// / 补全菜单：命令名蓝、参数提示与摘要灰阶分层；
	// 选中行 accent 指示符 + 随明暗的浅色底（整行反色太刺眼）。
	menuSelBg        = lipgloss.AdaptiveColor{Light: "#E8EDFF", Dark: "#252D45"}
	menuCursorStyle  = lipgloss.NewStyle().Foreground(accent).Bold(true).Background(menuSelBg)
	menuNameStyle    = lipgloss.NewStyle().Foreground(accent)
	menuNameSelStyle = lipgloss.NewStyle().Foreground(accent).Bold(true).Background(menuSelBg)
	menuHintStyle    = lipgloss.NewStyle().Foreground(dimColor)
	menuHintSelStyle = lipgloss.NewStyle().Foreground(dimColor).Background(menuSelBg)
	menuSumStyle     = lipgloss.NewStyle().Foreground(dimColor)
	menuSumSelStyle  = lipgloss.NewStyle().Foreground(inkColor).Background(menuSelBg)
	menuSelFill      = lipgloss.NewStyle().Background(menuSelBg)
)

// modeBadge 返回带色的模式标签，用于状态栏。
func modeBadge(m permMode) string {
	switch m {
	case modePlan:
		return planBadge.Render("plan")
	case modeAuto:
		return autoBadge.Render("auto")
	case modeYolo:
		return yoloBadge.Render("yolo")
	default:
		return reviewBadge.Render("review")
	}
}

func sp(s string) *string { return &s }

// markdown 代码块语法高亮的强调色（中等明度，亮/暗底都可见）。
const (
	mdKeyword = "#4D6BFE" // 关键字：蓝
	mdNeutral = "#7A8290" // 运算符/标点：中性灰
	mdComment = "#8D8D8D" // 注释灰
	mdTeal    = "#0E8FA8" // 数字/常量：青
	mdGreen   = "#16A571" // 字符串/函数：绿
)

// deepseekMarkdownStyle 按背景明暗选 glamour 亮/暗底样式，强调元素改 DeepSeek 蓝，
// 并重写代码配色——默认主题会把行内代码渲成红字、代码块在 CJK 上渲成红底
// （chroma 词法失败→error token 的红背景），这里换成蓝青中性系，且文字/底色随明暗反转。
func deepseekMarkdownStyle(dark bool) ansi.StyleConfig {
	s := styles.LightStyleConfig
	codeText, codeBg := "#2A2A2A", "#EEF1F8"
	if dark {
		s = styles.DarkStyleConfig
		codeText, codeBg = "#D8DCE4", "#1E2436"
	}

	// 标题 / 链接 / 强调 → DeepSeek 蓝。
	s.Heading.Color = sp(deepSeekBlue)
	s.H1.Color = sp(deepSeekBlue)
	s.H1.BackgroundColor = nil // 去掉 H1 的高亮底，免得和蓝字打架
	s.H2.Color = sp(deepSeekBlue)
	s.H3.Color = sp(deepSeekBlue)
	s.Strong.Color = sp(deepSeekBlue)
	s.Link.Color = sp(deepSeekBlue)

	// 行内代码：去红，中性字 + 随明暗的底色。
	s.Code.Color = sp(codeText)
	s.Code.BackgroundColor = sp(codeBg)

	// 代码块：无红的蓝青 chroma 调色板。
	cb := s.CodeBlock
	cb.Chroma = deepseekChroma(codeText, codeBg)
	s.CodeBlock = cb

	return s
}

// deepseekChroma 是代码块调色板：去掉一切红（尤其 error 的红底），关键字蓝、
// 字符串/函数绿、数字/常量青、运算符标点中性灰。CJK 等无法词法分析的内容会落到
// Text/Error，二者都设为随明暗的中性字色，于是渲染成干净的可读文字。
func deepseekChroma(text, bg string) *ansi.Chroma {
	return &ansi.Chroma{
		Text:             ansi.StylePrimitive{Color: sp(text)},
		Error:            ansi.StylePrimitive{Color: sp(text)}, // 关键：去掉红底
		Comment:          ansi.StylePrimitive{Color: sp(mdComment)},
		CommentPreproc:   ansi.StylePrimitive{Color: sp(mdKeyword)},
		Keyword:          ansi.StylePrimitive{Color: sp(mdKeyword)},
		KeywordReserved:  ansi.StylePrimitive{Color: sp(mdKeyword)},
		KeywordNamespace: ansi.StylePrimitive{Color: sp(mdKeyword)},
		KeywordType:      ansi.StylePrimitive{Color: sp(mdKeyword)},
		Operator:         ansi.StylePrimitive{Color: sp(mdNeutral)},
		Punctuation:      ansi.StylePrimitive{Color: sp(mdNeutral)},
		Name:             ansi.StylePrimitive{Color: sp(text)},
		NameBuiltin:      ansi.StylePrimitive{Color: sp(mdKeyword)},
		NameTag:          ansi.StylePrimitive{Color: sp(mdKeyword)},
		NameAttribute:    ansi.StylePrimitive{Color: sp(mdTeal)},
		NameClass:        ansi.StylePrimitive{Color: sp(mdKeyword)},
		NameConstant:     ansi.StylePrimitive{Color: sp(mdTeal)},
		NameFunction:     ansi.StylePrimitive{Color: sp(mdGreen)},
		LiteralString:    ansi.StylePrimitive{Color: sp(mdGreen)},
		LiteralNumber:    ansi.StylePrimitive{Color: sp(mdTeal)},
		GenericDeleted:   ansi.StylePrimitive{Color: sp(mdNeutral)},
		GenericInserted:  ansi.StylePrimitive{Color: sp(mdGreen)},
		Background:       ansi.StylePrimitive{BackgroundColor: sp(bg)},
	}
}
