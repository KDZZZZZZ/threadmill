package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// 调色板：墨织单色，全部 R==G==B（由 TestViewIsAchromatic 守护该不变量）。
// 不设任何色相；ink 是唯一"强调"，只给 brand、active、光标与错误反白。
var (
	colorCanvas     = lipgloss.Color("#0B0B0B") // 墨底
	colorRaise      = lipgloss.Color("#1B1B1B") // 节点织物面
	colorLine       = lipgloss.Color("#2A2A2A") // 发丝线
	colorLineHi     = lipgloss.Color("#3D3D3D") // 呼吸亮档
	colorFaint      = lipgloss.Color("#5A5A5A") // 弱提示
	colorMuted      = lipgloss.Color("#858585") // 次文本
	colorDim        = lipgloss.Color("#ADADAD") // 正文
	colorForeground = lipgloss.Color("#D8D8D8") // 亮正文
	colorInk        = lipgloss.Color("#F5F5F5") // 白墨强调
)

func surfaceStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Background(colorCanvas)
}

// stitchBorder 车缝线边框：横竖皆针脚虚线，角以线结收针。
func stitchBorder() lipgloss.Border {
	return lipgloss.Border{
		Top:         "┄",
		Bottom:      "┄",
		Left:        "┆",
		Right:       "┆",
		TopLeft:     "·",
		TopRight:    "·",
		BottomLeft:  "·",
		BottomRight: "·",
	}
}

func inputBase(line lipgloss.TerminalColor) lipgloss.Style {
	return surfaceStyle().
		BorderStyle(stitchBorder()).
		BorderBottom(true).
		BorderForeground(line)
}

// brandStripe 三根明度渐变的线，是单色世界里的品牌纹样。
func brandStripe() string {
	return lipgloss.JoinHorizontal(
		lipgloss.Top,
		lipgloss.NewStyle().Foreground(colorFaint).Background(colorCanvas).Bold(true).Render("▮"),
		lipgloss.NewStyle().Foreground(colorMuted).Background(colorCanvas).Bold(true).Render("▮"),
		lipgloss.NewStyle().Foreground(colorInk).Background(colorCanvas).Bold(true).Render("▮"),
	)
}

// modeTabs 把视图指示渲染成页签对：当前页白墨粗体下划线，另一页隐入 faint。
func modeTabs(active viewMode) string {
	on := lipgloss.NewStyle().
		Foreground(colorInk).
		Background(colorCanvas).
		Bold(true).
		Underline(true)
	off := lipgloss.NewStyle().Foreground(colorFaint).Background(colorCanvas)
	chat, graph := off.Render("CHAT"), off.Render("GRAPH")
	if active == viewChat {
		chat = on.Render("CHAT")
	} else {
		graph = on.Render("GRAPH")
	}
	return chat + off.Render(" · ") + graph
}

// weaveMotif 空态织物纹样：一列明度渐变的织块。
func weaveMotif() string {
	shades := []lipgloss.TerminalColor{colorFaint, colorMuted, colorDim, colorInk}
	blocks := []string{"▚", "▞", "▚", "▞"}
	parts := make([]string, len(blocks))
	for i, block := range blocks {
		parts[i] = lipgloss.NewStyle().
			Foreground(shades[i]).
			Background(colorCanvas).
			Render(block)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// errorStyle 错误不靠色相：墨底上的反白粗体条。
func errorStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(colorCanvas).
		Background(colorInk).
		Bold(true)
}

// flowEdge 渲染任务连线：active 时针脚随相位流向箭头，pending 是静态针脚，
// 其余常态。frames 两帧宽度必须一致，流动不改变布局。
func flowEdge(phase uint64, target taskNodeStatus, frames [2]string, pend, done string) string {
	switch target {
	case taskActive:
		return surfaceStyle().Foreground(colorInk).Render(frames[phase%2])
	case taskPending:
		return surfaceStyle().Foreground(colorFaint).Render(pend)
	default:
		return surfaceStyle().Foreground(colorMuted).Render(done)
	}
}

// breatheColor active 节点描边随相位在发丝线→亮档→白墨间呼吸。
func breatheColor(phase uint64) lipgloss.TerminalColor {
	switch phase % 3 {
	case 0:
		return colorLine
	case 1:
		return colorLineHi
	default:
		return colorInk
	}
}

// weaveMask 织入转场:frame 为 2 时先织奇数行(偶数行留墨底),为 1 时补织偶数行。
// 遮罩行替换为等宽墨底空白,几何不变,所以不会惊动滚动位置。
func weaveMask(view string, frame int) string {
	lines := strings.Split(view, "\n")
	for i, line := range lines {
		if i%2 == frame%2 {
			lines[i] = surfaceStyle().Width(lipgloss.Width(line)).Render("")
		}
	}
	return strings.Join(lines, "\n")
}
