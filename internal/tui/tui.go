// Package tui 是 threadmill 的默认交互界面：消息区、输入框、状态栏。
//
// 布局参考 Pi 的「滚动 transcript + 底部编辑器 + footer」。
// 组件用法来自 Bubble Tea 官方 chat 示例：
// https://github.com/charmbracelet/bubbletea/blob/v1.3.10/examples/chat/main.go
package tui

import (
	"context"
	"fmt"
	"strings"

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
	width    int
	height   int
}

func newModel(chat Chat, info Info) model {
	input := textarea.New()
	input.Placeholder = "发给 manager…"
	input.Focus()
	input.Prompt = "┃ "
	input.ShowLineNumbers = false
	input.SetHeight(3)
	input.FocusedStyle.CursorLine = lipgloss.NewStyle()
	input.KeyMap.InsertNewline.SetEnabled(false)

	vp := viewport.New(80, 20)
	vp.SetContent("threadmill")

	return model{
		chat:     chat,
		info:     info,
		viewport: vp,
		input:    input,
		inflight: map[string]int{},
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
	return textarea.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil
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
		m.applyEvent(msg)
		m.refresh()
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
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseDelta && ev.AgentID == "manager":
		m.appendAssistant(ev.Delta)
		m.streamed = true
	case ev.Kind == event.KindTool:
		line := ev.Name + " " + string(ev.Phase)
		if ev.AgentID != "" {
			line = fmt.Sprintf("[%s] tool %s %s", ev.AgentID, ev.Name, ev.Phase)
		} else {
			line = fmt.Sprintf("tool %s", line)
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

func (m *model) layout() {
	m.input.SetWidth(m.width)
	statusH := lipgloss.Height(m.statusLine())
	gap := 1
	h := m.height - m.input.Height() - statusH - gap
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
	wrap := lipgloss.NewStyle().Width(width)
	parts := make([]string, 0, len(m.items))
	for _, it := range m.items {
		parts = append(parts, wrap.Render(renderItem(it)))
	}
	m.viewport.SetContent(strings.Join(parts, "\n"))
	m.viewport.GotoBottom()
}

func (m model) View() string {
	return lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		m.input.View(),
		m.statusLine(),
	)
}

func (m model) statusLine() string {
	modelName := m.info.Model
	if modelName == "" {
		modelName = "-"
	}
	line := fmt.Sprintf(
		" %s  %s  token %d  tasks %d ",
		m.info.Root,
		modelName,
		m.tokens,
		m.runningTasks(),
	)
	style := lipgloss.NewStyle().Reverse(true)
	if m.width > 0 {
		style = style.Width(m.width)
	}
	return style.Render(line)
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

func renderItem(it item) string {
	switch it.kind {
	case itemUser:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render("you: ") + it.text
	case itemReport:
		return lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("3")).
			Padding(0, 1).
			Render(it.text)
	case itemActivity:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(it.text)
	default:
		return it.text
	}
}
