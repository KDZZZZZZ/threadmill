// Package tui 是 threadmill 的默认交互界面：顶栏、消息区、输入框、状态栏。
//
// 布局参考 Pi 的「滚动 transcript + 底部编辑器 + footer」。
// 观感走工业粗线：反相顶栏/底栏、ThickBorder 输入框、消息不加圆角框。
// Lip Gloss：https://pkg.go.dev/github.com/charmbracelet/lipgloss@v1.1.0
package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/KDZZZZZZ/threadmill/internal/event"
)

// Chat 是 TUI 需要的会话能力。
type Chat interface {
	Send(string)
	Cancel() bool
}

// Info 是状态栏上的工作区与模型名。
type Info struct {
	Root  string
	Model string
}

// OutputMsg 是经理整段回复或任务报告。
type OutputMsg struct {
	Text string
}

type itemKind int

const (
	itemUser itemKind = iota
	itemAssistant
	itemReport
	itemActivity
	itemThinking
	itemError
)

type item struct {
	kind itemKind
	text string
}

type model struct {
	chat     Chat
	info     Info
	viewport viewport.Model
	input    textarea.Model
	items    []item
	streamed bool
	tokens   int
	inflight map[string]int
	spin     spinner.Model
	width    int
	height   int
}

func newModel(chat Chat, info Info) model {
	input := textarea.New()
	input.Placeholder = "发给 manager"
	input.Focus()
	input.Prompt = "> "
	input.ShowLineNumbers = false
	input.SetHeight(3)
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.FocusedStyle.Placeholder = lipgloss.NewStyle().Foreground(colorDim)
	input.FocusedStyle.Prompt = lipgloss.NewStyle().Bold(true)
	input.BlurredStyle.Prompt = lipgloss.NewStyle().Foreground(colorDim)
	// Border 写在 Base 上：https://pkg.go.dev/github.com/charmbracelet/bubbles@v1.0.0/textarea
	input.FocusedStyle.Base = lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(colorInk)
	input.BlurredStyle.Base = lipgloss.NewStyle().
		Border(lipgloss.ThickBorder()).
		BorderForeground(colorDim)
	input.KeyMap.InsertNewline.SetEnabled(false)

	vp := viewport.New(80, 20)

	spin := spinner.New(spinner.WithSpinner(spinner.Line))

	return model{
		chat:     chat,
		info:     info,
		viewport: vp,
		input:    input,
		inflight: map[string]int{},
		spin:     spin,
	}
}

// NewProgram 构造全屏 TUI。调用方应在 Run 前把 Program.Send 接到会话事件上。
func NewProgram(ctx context.Context, chat Chat, info Info) *tea.Program {
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if ctx != nil {
		opts = append(opts, tea.WithContext(ctx))
	}
	return tea.NewProgram(newModel(chat, info), opts...)
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spin.Tick)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if !m.busy() {
			return m, nil
		}
		m.refresh()
		return m, cmd
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc:
			if m.chat != nil {
				m.chat.Cancel()
			}
			return m, nil
		case tea.KeyEnter:
			return m.submit()
		}
	case event.RuntimeEvent:
		wasBusy := m.busy()
		m.applyEvent(msg)
		m.refresh()
		if m.busy() && !wasBusy {
			return m, m.spin.Tick
		}
		return m, nil
	case OutputMsg:
		m.applyOutput(msg.Text)
		m.refresh()
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) submit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if line == "" {
		return m, nil
	}
	if line == "/quit" {
		return m, tea.Quit
	}
	m.items = append(m.items, item{kind: itemUser, text: line})
	if m.chat != nil {
		m.chat.Send(line)
	}
	m.refresh()
	return m, nil
}

func (m *model) applyEvent(ev event.RuntimeEvent) {
	switch {
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseStart && ev.AgentID == "manager":
		m.items = append(m.items, item{kind: itemThinking, text: "[manager] 思考中"})
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseDelta && ev.AgentID == "manager":
		m.dropThinking()
		m.appendAssistant(ev.Delta)
		m.streamed = true
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Err != "":
		who := ev.AgentID
		if who == "" {
			who = "model"
		}
		m.dropThinking()
		m.items = append(m.items, item{kind: itemError, text: fmt.Sprintf("[%s] %s", who, ev.Err)})
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.AgentID == "manager":
		m.dropThinking()
	case ev.Kind == event.KindTool:
		line := ev.Name + " " + string(ev.Phase)
		if ev.AgentID != "" {
			line = fmt.Sprintf("[%s] tool %s %s", ev.AgentID, ev.Name, ev.Phase)
		} else {
			line = fmt.Sprintf("tool %s", line)
		}
		if ev.Err != "" {
			line += ": " + ev.Err
			m.items = append(m.items, item{kind: itemError, text: line})
			break
		}
		m.items = append(m.items, item{kind: itemActivity, text: line})
	}
	if ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Name != "" && m.info.Model == "" {
		m.info.Model = ev.Name
	}
	if ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Tokens > 0 {
		m.tokens += ev.Tokens
	}
	switch ev.Phase {
	case event.PhaseStart:
		m.inflight[ev.AgentID]++
	case event.PhaseEnd:
		m.inflight[ev.AgentID]--
		if m.inflight[ev.AgentID] <= 0 {
			delete(m.inflight, ev.AgentID)
		}
	}
}

func (m *model) appendAssistant(delta string) {
	n := len(m.items)
	if n > 0 && m.items[n-1].kind == itemAssistant {
		m.items[n-1].text += delta
		return
	}
	m.items = append(m.items, item{kind: itemAssistant, text: delta})
}

func (m *model) applyOutput(text string) {
	if strings.HasPrefix(text, "[任务报告]") {
		m.items = append(m.items, item{kind: itemReport, text: text})
		return
	}
	if m.streamed {
		m.streamed = false
		return
	}
	if text == "" {
		return
	}
	m.appendAssistant(text)
}

func (m *model) dropThinking() {
	keep := make([]item, 0, len(m.items))
	for _, it := range m.items {
		if it.kind != itemThinking {
			keep = append(keep, it)
		}
	}
	m.items = keep
}

func (m model) busy() bool {
	for _, n := range m.inflight {
		if n > 0 {
			return true
		}
	}
	return false
}

func (m *model) layout() {
	m.input.SetWidth(m.width)
	headerH := lipgloss.Height(m.headerLine())
	statusH := lipgloss.Height(m.statusLine())
	inputH := lipgloss.Height(m.input.View())
	h := m.height - headerH - inputH - statusH
	if h < 1 {
		h = 1
	}
	m.viewport.Width = m.width
	m.viewport.Height = h
	m.refresh()
}

func (m *model) refresh() {
	width := m.viewport.Width
	if width <= 0 {
		width = 80
	}
	if len(m.items) == 0 {
		hint := lipgloss.NewStyle().Foreground(colorDim).Width(width).Render(
			"空。Enter 发送  Esc 取消  Ctrl+C 退出",
		)
		m.viewport.SetContent(hint)
		return
	}
	parts := make([]string, 0, len(m.items))
	for _, it := range m.items {
		parts = append(parts, renderItem(it, width))
	}
	m.viewport.SetContent(strings.Join(parts, "\n"))
	m.viewport.GotoBottom()
}

func (m model) View() string {
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.headerLine(),
		m.viewport.View(),
		m.input.View(),
		m.statusLine(),
	)
}

func (m model) headerLine() string {
	modelName := m.info.Model
	if modelName == "" {
		modelName = "-"
	}
	text := "threadmill  " + modelName + "  " + shortenPath(m.info.Root, 40)
	return chromeBar(m.width).Bold(true).Render(text)
}

func (m model) statusLine() string {
	modelName := m.info.Model
	if modelName == "" {
		modelName = "-"
	}
	left := fmt.Sprintf(
		" %s  %s  token %d  tasks %d ",
		m.info.Root,
		modelName,
		m.tokens,
		m.runningTasks(),
	)
	if m.busy() {
		left = fmt.Sprintf(" %s 思考  %s", m.spin.View(), left)
	}
	right := " enter send | esc cancel | ctrl+c quit "
	line := left
	if m.width > 0 {
		pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
		if pad < 1 {
			pad = 1
		}
		line = left + strings.Repeat(" ", pad) + right
	} else {
		line = left + right
	}
	return chromeBar(m.width).Render(line)
}

func (m model) runningTasks() int {
	seen := map[string]struct{}{}
	for id, n := range m.inflight {
		if n <= 0 {
			continue
		}
		key := taskKey(id)
		if key == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	return len(seen)
}

func taskKey(agentID string) string {
	i := strings.IndexByte(agentID, ':')
	if i <= 0 {
		return ""
	}
	return agentID[:i]
}

var (
	colorInk   = lipgloss.Color("7")
	colorDim   = lipgloss.Color("8")
	colorError = lipgloss.Color("1")
	colorWarn  = lipgloss.Color("3")
)

func chromeBar(width int) lipgloss.Style {
	style := lipgloss.NewStyle().Reverse(true).Padding(0, 1)
	if width > 0 {
		style = style.Width(width)
	}
	return style
}

func renderItem(it item, width int) string {
	switch it.kind {
	case itemUser:
		return lipgloss.NewStyle().Bold(true).Width(width).Render("> " + it.text)
	case itemAssistant:
		return lipgloss.NewStyle().Width(width).Render(it.text)
	case itemReport:
		return lipgloss.NewStyle().Foreground(colorWarn).Width(width).Render("==\n" + it.text)
	case itemActivity:
		return lipgloss.NewStyle().Foreground(colorDim).Width(width).Render("* " + it.text)
	case itemThinking:
		return lipgloss.NewStyle().Foreground(colorDim).Width(width).Render(it.text)
	case itemError:
		return lipgloss.NewStyle().Foreground(colorError).Bold(true).Width(width).Render("! " + it.text)
	default:
		return lipgloss.NewStyle().Width(width).Render(it.text)
	}
}

func shortenPath(p string, max int) string {
	if max <= 1 || p == "" {
		return p
	}
	runes := []rune(p)
	if len(runes) <= max {
		return p
	}
	return "…" + string(runes[len(runes)-(max-1):])
}
