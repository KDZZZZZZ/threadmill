// Package tui 是 threadmill 的默认交互界面：聊天、实时 Graph、状态栏与输入框。
//
// 布局沿用「滚动 transcript + 底部 dock」，Tab 在 CHAT / GRAPH 两帧间切换。
// 视觉语言为「墨织」：纯灰阶无彩色，艺术性来自针脚纹样（┄┆）、
// 流动连线、节点呼吸与 Tab 的织入转场，唯一的强调是白墨。
// Lip Gloss：https://pkg.go.dev/github.com/charmbracelet/lipgloss@v1.1.0
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/KDZZZZZZ/threadmill/internal/event"
)

// Chat 是 TUI 需要的 manager 能力。
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
	kind  itemKind
	text  string
	parts []string
}

type viewMode int

const (
	viewChat viewMode = iota
	viewGraph
)

type taskView struct {
	id         string
	outcome    string
	seenRoles  map[string]bool
	failedRole map[string]bool
}

type model struct {
	chat            Chat
	info            Info
	viewport        viewport.Model
	input           textarea.Model
	items           []item
	mode            viewMode
	tasks           map[string]taskView
	taskOrder       []string
	streamed        []string
	tokens          int
	inflight        map[string]int
	activity        map[string]runtimeActivity
	activityN       uint64
	framePending    bool
	transcriptDirty bool
	thinking        bool
	spin            spinner.Model
	phase           uint64
	weave           int
	width           int
	height          int
}

func newModel(chat Chat, info Info) model {
	input := textarea.New()
	input.Placeholder = "发给 manager"
	input.Focus()
	input.Prompt = "❯ "
	input.ShowLineNumbers = false
	input.SetHeight(1)
	input.Cursor.Style = lipgloss.NewStyle().
		Foreground(colorCanvas).
		Background(colorInk)
	input.FocusedStyle.Base = inputBase(colorInk)
	input.FocusedStyle.CursorLine = surfaceStyle()
	input.FocusedStyle.Placeholder = surfaceStyle().Foreground(colorFaint)
	input.FocusedStyle.Prompt = surfaceStyle().Foreground(colorInk).Bold(true)
	input.FocusedStyle.Text = surfaceStyle().Foreground(colorForeground)
	input.FocusedStyle.EndOfBuffer = surfaceStyle()
	input.BlurredStyle.Base = inputBase(colorLine)
	input.BlurredStyle.CursorLine = surfaceStyle()
	input.BlurredStyle.Placeholder = surfaceStyle().Foreground(colorFaint)
	input.BlurredStyle.Prompt = surfaceStyle().Foreground(colorMuted)
	input.BlurredStyle.Text = surfaceStyle().Foreground(colorDim)
	input.BlurredStyle.EndOfBuffer = surfaceStyle()
	input.KeyMap.InsertNewline.SetEnabled(false)

	vp := viewport.New(80, 20)

	// 梭子绕线：四帧旋转角，兼作全局纹样相位的节拍器。
	spin := spinner.New(spinner.WithSpinner(spinner.Spinner{
		Frames: []string{"◜", "◝", "◞", "◟"},
		FPS:    time.Second / 8,
	}))
	spin.Style = lipgloss.NewStyle().Foreground(colorInk)

	return model{
		chat:     chat,
		info:     info,
		viewport: vp,
		input:    input,
		tasks:    map[string]taskView{},
		inflight: map[string]int{},
		activity: map[string]runtimeActivity{},
		spin:     spin,
	}
}

// NewProgram 构造全屏 TUI。调用方应在 Run 前把 Program.Send 接到 manager 事件上。
func NewProgram(ctx context.Context, chat Chat, info Info) *tea.Program {
	opts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
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
		m.weave = 0
		m.layout()
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if !m.busy() {
			return m, nil
		}
		// spinner 的节拍同时驱动纹样相位:流动连线与呼吸描边。
		m.phase++
		if m.mode == viewGraph {
			m.refresh()
		}
		return m, cmd
	case transcriptFrameMsg:
		m.framePending = false
		if m.transcriptDirty && m.mode == viewChat {
			m.refresh()
		}
		m.transcriptDirty = false
		return m, nil
	case weaveTickMsg:
		if m.weave > 0 {
			m.weave--
			if m.weave > 0 {
				return m, nextWeaveFrame()
			}
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyTab:
			if m.mode == viewChat {
				m.mode = viewGraph
			} else {
				m.mode = viewChat
			}
			m.layout()
			if m.mode == viewGraph {
				m.viewport.GotoTop()
			} else {
				m.viewport.GotoBottom()
			}
			m.weave = weaveFrames
			return m, nextWeaveFrame()
		case tea.KeyEsc:
			if m.chat != nil {
				m.chat.Cancel()
			}
			return m, nil
		case tea.KeyEnter:
			return m.submit()
		case tea.KeyPgUp, tea.KeyPgDown:
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
	case tea.MouseMsg:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case event.RuntimeEvent:
		return m, m.handleRuntimeEvent(msg)
	case OutputMsg:
		if m.applyOutput(msg.Text) || m.transcriptDirty {
			m.refresh()
			m.transcriptDirty = false
		}
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
	m.transcriptDirty = false
	m.viewport.GotoBottom()
	return m, nil
}

func (m *model) layout() {
	follow := m.viewport.AtBottom()
	offset := m.viewport.YOffset
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
	if m.mode == viewChat {
		chatWidth, _, graphHeight, sideBySide := chatGraphGeometry(m.width, h)
		m.viewport.Width = chatWidth
		if !sideBySide && graphHeight > 0 {
			m.viewport.Height = max(1, h-graphHeight-1)
		}
	}
	m.refreshAt(follow, offset)
	m.transcriptDirty = false
}

func (m *model) refresh() {
	m.refreshAt(m.viewport.AtBottom(), m.viewport.YOffset)
}

func (m *model) refreshAt(follow bool, offset int) {
	width := m.viewport.Width
	if width <= 0 {
		width = 80
	}
	if m.mode == viewGraph {
		m.viewport.SetContent(m.graphView(width))
		if follow {
			m.viewport.GotoBottom()
		} else {
			m.viewport.SetYOffset(offset)
		}
		return
	}
	if len(m.items) == 0 {
		left, contentWidth := transcriptGeometry(width)
		block := surfaceStyle().
			MarginLeft(left).
			Width(contentWidth).
			Align(lipgloss.Center)
		motif := block.Render(weaveMotif())
		hint := block.Foreground(colorFaint).Render("SESSION READY · 输入任务后按 Enter")
		m.viewport.SetContent("\n" + motif + "\n" + hint)
		m.viewport.SetYOffset(offset)
		return
	}
	parts := make([]string, 0, len(m.items))
	for _, it := range m.items {
		parts = append(parts, renderItem(it, width))
	}
	m.viewport.SetContent("\n" + strings.Join(parts, "\n\n"))
	if follow {
		m.viewport.GotoBottom()
	} else {
		m.viewport.SetYOffset(offset)
	}
}

func (m model) graphView(width int) string {
	left, contentWidth := transcriptGeometry(width)
	title := lipgloss.NewStyle().
		Foreground(colorInk).
		Background(colorCanvas).
		Bold(true).
		Render("GRAPH")
	sub := surfaceStyle().
		Foreground(colorFaint).
		Render(fmt.Sprintf("  ┄┄  LIVE EXECUTION · %d NODES", len(m.taskOrder)*3))
	header := surfaceStyle().
		MarginLeft(left).
		Width(contentWidth).
		Render(title + sub)
	if len(m.taskOrder) == 0 {
		empty := surfaceStyle().
			Foreground(colorFaint).
			MarginLeft(left).
			Width(contentWidth).
			Render("NO TASKS YET · 任务启动后会在这里展开")
		return "\n" + header + "\n\n" + empty
	}

	rows := make([]string, 0, len(m.taskOrder)+1)
	rows = append(rows, header)
	for _, taskID := range m.taskOrder {
		rows = append(rows, m.renderTaskGraph(m.tasks[taskID], left, contentWidth))
	}
	return "\n" + strings.Join(rows, "\n\n")
}

func (m model) renderTaskGraph(task taskView, left, width int) string {
	roles := []string{"planner", "executor", "verifier"}
	statuses := make([]taskNodeStatus, len(roles))
	nodes := make([]string, len(roles))
	for i, role := range roles {
		statuses[i] = m.taskRoleStatus(task, role)
		nodes[i] = renderGraphNode(task.id, role, statuses[i], m.phase)
	}
	arrows := []string{
		flowEdge(m.phase, statuses[1], [2]string{" ─╌▶ ", " ╌─▶ "}, " ┄┄▶ ", " ──▶ "),
		flowEdge(m.phase, statuses[2], [2]string{" ─╌▶ ", " ╌─▶ "}, " ┄┄▶ ", " ──▶ "),
	}
	row := lipgloss.JoinHorizontal(
		lipgloss.Center,
		nodes[0], arrows[0], nodes[1], arrows[1], nodes[2],
	)
	if lipgloss.Width(row) > width {
		dimDown := surfaceStyle().Foreground(colorMuted).Render("↓")
		inkDown := surfaceStyle().Foreground(colorInk).Render("↓")
		row = lipgloss.JoinVertical(
			lipgloss.Center,
			nodes[0],
			dimDown,
			nodes[1],
			inkDown,
			nodes[2],
		)
	}
	return lipgloss.NewStyle().MarginLeft(left).Render(row)
}

func (m model) View() string {
	view := lipgloss.JoinVertical(
		lipgloss.Left,
		m.headerLine(),
		m.bodyView(),
		m.statusLine(),
		m.input.View(),
	)
	if m.width <= 0 || m.height <= 0 {
		return view
	}
	rendered := surfaceStyle().Width(m.width).Height(m.height).Render(view)
	if m.weave > 0 {
		rendered = weaveMask(rendered, m.weave)
	}
	return rendered
}

func (m model) bodyView() string {
	if m.mode == viewGraph || m.width <= 0 {
		return m.viewport.View()
	}

	bodyHeight := m.height - lipgloss.Height(m.headerLine()) -
		lipgloss.Height(m.statusLine()) - lipgloss.Height(m.input.View())
	bodyHeight = max(1, bodyHeight)
	_, graphWidth, graphHeight, sideBySide := chatGraphGeometry(m.width, bodyHeight)
	if graphWidth == 0 || graphHeight == 0 {
		return m.viewport.View()
	}
	graph := m.miniGraphView(graphWidth, graphHeight)

	if sideBySide {
		graph = surfaceStyle().Width(graphWidth).Height(bodyHeight).Render(graph)
		gap := surfaceStyle().Width(2).Height(bodyHeight).Render("")
		return lipgloss.JoinHorizontal(lipgloss.Top, m.viewport.View(), gap, graph)
	}

	graph = surfaceStyle().Width(m.width).Align(lipgloss.Right).Render(graph)
	return lipgloss.JoinVertical(lipgloss.Left, graph, surfaceStyle().Render(""), m.viewport.View())
}

func chatGraphGeometry(width, height int) (chatWidth, graphWidth, graphHeight int, sideBySide bool) {
	if width < 20 || height < 5 {
		return width, 0, 0, false
	}
	if width >= 90 {
		// 42 列起步：保证三节点 + 带空隙箭头（3×12 + 2×3）不换行、不挤压。
		graphWidth = min(52, max(42, width*37/100))
		graphHeight = min(height, max(8, graphWidth*5/16))
		return max(1, width-graphWidth-2), graphWidth, graphHeight, true
	}

	graphWidth = min(32, max(20, width*52/100))
	graphWidth = min(width, graphWidth)
	graphHeight = min(max(3, height-2), max(5, graphWidth*5/16))
	return width, graphWidth, graphHeight, false
}

func (m model) miniGraphView(width, height int) string {
	tasks := m.taskOrder
	if len(tasks) == 0 {
		idle := taskView{id: "waiting", seenRoles: map[string]bool{}, failedRole: map[string]bool{}}
		return placeMiniGraph(renderMiniTaskGraph(m, idle, width), width, height)
	}

	rows := make([]string, 0, len(tasks))
	for i := len(tasks) - 1; i >= 0; i-- {
		row := renderMiniTaskGraph(m, m.tasks[tasks[i]], width)
		candidate := strings.Join(append([]string{row}, rows...), "\n\n")
		if len(rows) > 0 && lipgloss.Height(candidate) > height {
			break
		}
		rows = append([]string{row}, rows...)
	}
	return placeMiniGraph(strings.Join(rows, "\n\n"), width, height)
}

func placeMiniGraph(graph string, width, height int) string {
	return surfaceStyle().
		Width(width).
		Height(height).
		Align(lipgloss.Center).
		Render(graph)
}

func renderMiniTaskGraph(m model, task taskView, width int) string {
	roles := []string{"planner", "executor", "verifier"}
	statuses := make([]taskNodeStatus, len(roles))
	for i, role := range roles {
		statuses[i] = m.taskRoleStatus(task, role)
	}
	// 三节点（含边框 12 列）+ 两侧留隙的箭头（各 3 列）= 3×nodeW + 12。
	nodeW := min(12, (width-12)/3)
	if width < 30 || nodeW < 9 {
		tokens := make([]string, 0, len(roles))
		for i, role := range roles {
			tokens = append(tokens, renderMiniGraphToken(role, statuses[i], m.phase))
			if i < len(roles)-1 {
				tokens = append(tokens, flowEdge(m.phase, statuses[i+1], [2]string{"→", "⇢"}, "→", "→"))
			}
		}
		chain := lipgloss.JoinHorizontal(lipgloss.Center, tokens...)
		detail := surfaceStyle().Foreground(colorFaint).Render(sanitizeText(task.id))
		return lipgloss.JoinVertical(lipgloss.Center, chain, detail)
	}

	nodes := make([]string, 0, len(roles))
	for i, role := range roles {
		nodes = append(nodes, renderMiniGraphNode(role, statuses[i], nodeW, m.phase))
	}
	arrow := func(target taskNodeStatus) string {
		return flowEdge(m.phase, target, [2]string{" → ", " ⇢ "}, " → ", " → ")
	}
	chain := lipgloss.JoinHorizontal(
		lipgloss.Center,
		nodes[0], arrow(statuses[1]), nodes[1], arrow(statuses[2]), nodes[2],
	)
	caption := surfaceStyle().Foreground(colorFaint).Render(sanitizeText(task.id))
	return lipgloss.JoinVertical(lipgloss.Center, chain, caption)
}

func (m model) headerLine() string {
	modelName := sanitizeText(m.info.Model)
	if modelName == "" {
		modelName = "-"
	}
	brand := lipgloss.NewStyle().
		Foreground(colorInk).
		Background(colorCanvas).
		Bold(true).
		Render("THREADMILL")
	left := brand + " " + brandStripe()
	right := modeTabs(m.mode) + lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorCanvas).
		Render(" · "+modelName+" · "+shortenPath(sanitizeText(m.info.Root), 40))
	return surfaceStyle().Width(m.width).Render(joinEnds(left, right, m.width, 2))
}

func (m model) statusLine() string {
	metrics := fmt.Sprintf(
		"token %d · tasks %d",
		m.tokens,
		m.runningTasks(),
	)
	left := lipgloss.NewStyle().
		Foreground(colorMuted).
		Background(colorCanvas).
		Render(metrics)
	if m.busy() {
		activeText := m.spin.View() + " ACTIVE · " + m.currentActivity()
		if candidate := activeText + " · " + metrics; lipgloss.Width(candidate) <= m.width {
			activeText = candidate
		}
		activeText = ansi.Truncate(activeText, max(1, m.width), "…")
		active := lipgloss.NewStyle().
			Foreground(colorInk).
			Background(colorCanvas).
			Bold(true).
			Render(activeText)
		left = active
	}
	right := lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorCanvas).
		Render("TAB " + m.otherModeLabel() + " · ENTER SEND · ESC CANCEL · ^C QUIT")
	return surfaceStyle().
		Width(m.width).
		BorderStyle(stitchBorder()).
		BorderTop(true).
		BorderForeground(colorLine).
		Render(joinEnds(left, right, m.width, 2))
}

func (m model) otherModeLabel() string {
	if m.mode == viewGraph {
		return "CHAT"
	}
	return "GRAPH"
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
	if !strings.HasPrefix(agentID, "task-") {
		return ""
	}
	i := strings.IndexByte(agentID, ':')
	if i < 0 {
		return agentID
	}
	return agentID[:i]
}

type taskNodeStatus int

const (
	taskPending taskNodeStatus = iota
	taskActive
	taskDone
	taskFailed
	taskCanceled
)

func (m model) taskRoleStatus(task taskView, role string) taskNodeStatus {
	if m.inflight[task.id+":"+role] > 0 {
		return taskActive
	}
	if task.failedRole[role] {
		return taskFailed
	}
	if !task.seenRoles[role] {
		return taskPending
	}

	lastSeen := role
	for _, candidate := range []string{"planner", "executor", "verifier"} {
		if task.seenRoles[candidate] {
			lastSeen = candidate
		}
	}
	if task.outcome == "failed" && role == lastSeen {
		return taskFailed
	}
	if task.outcome == "canceled" && role == lastSeen {
		return taskCanceled
	}
	if task.outcome == "active" && role == lastSeen {
		return taskActive
	}
	return taskDone
}

// graphNodeVisual 给出节点状态符号、文字色、描边色。
// pending=弱针脚节点、run=白墨呼吸描边、done=常态面+亮符号、failed=反白、canceled=熄灭。
func graphNodeVisual(
	status taskNodeStatus,
	phase uint64,
) (marker string, color, border lipgloss.TerminalColor) {
	switch status {
	case taskActive:
		return "◌", colorInk, breatheColor(phase)
	case taskDone:
		return "✓", colorForeground, colorLine
	case taskFailed:
		return "▲", colorInk, colorInk
	case taskCanceled:
		return "■", colorMuted, colorLine
	default:
		return "○", colorMuted, colorLine
	}
}

// nodeBorder pending 节点用针脚虚线框，其余节点用常态实线框。
func nodeBorder(status taskNodeStatus) lipgloss.Border {
	if status == taskPending {
		return stitchBorder()
	}
	return lipgloss.NormalBorder()
}

func renderGraphNode(taskID, role string, status taskNodeStatus, phase uint64) string {
	marker, color, border := graphNodeVisual(status, phase)
	labelStyle := lipgloss.NewStyle().
		Foreground(color).
		Background(colorRaise).
		Bold(true)
	if status == taskFailed {
		labelStyle = errorStyle()
	}
	label := labelStyle.Render(marker + " " + strings.ToUpper(role))
	detail := lipgloss.NewStyle().Foreground(colorFaint).Background(colorRaise).Render(sanitizeText(taskID))
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Background(colorRaise).
		BorderStyle(nodeBorder(status)).
		BorderForeground(border).
		Padding(0, 1).
		Width(14).
		Render(label + "\n" + detail)
}

func renderMiniGraphNode(role string, status taskNodeStatus, width int, phase uint64) string {
	marker, color, border := graphNodeVisual(status, phase)
	labelStyle := lipgloss.NewStyle().
		Foreground(color).
		Background(colorRaise).
		Bold(true).
		Width(width).
		Align(lipgloss.Center)
	if status == taskFailed {
		labelStyle = errorStyle().Width(width).Align(lipgloss.Center)
	}
	label := labelStyle.Render(marker + " " + strings.ToUpper(role))
	detail := lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorRaise).
		Width(width).
		Align(lipgloss.Center).
		Render(miniStatusWord(status))
	return lipgloss.NewStyle().
		Background(colorRaise).
		BorderStyle(nodeBorder(status)).
		BorderForeground(border).
		Width(width).
		Render(label + "\n" + detail)
}

// miniStatusWord 节点第二行的状态词，替代重复三份的 task id。
func miniStatusWord(status taskNodeStatus) string {
	switch status {
	case taskActive:
		return "run"
	case taskDone:
		return "done"
	case taskFailed:
		return "fail"
	case taskCanceled:
		return "stop"
	default:
		return "wait"
	}
}

func renderMiniGraphToken(role string, status taskNodeStatus, phase uint64) string {
	marker, color, _ := graphNodeVisual(status, phase)
	label := "PLAN"
	switch role {
	case "executor":
		label = "EXEC"
	case "verifier":
		label = "VERIFY"
	}
	return surfaceStyle().Foreground(color).Bold(true).Render(marker + " " + label)
}

func renderItem(it item, width int) string {
	if len(it.parts) > 0 {
		it.text += strings.Join(it.parts, "")
	}
	it.text = sanitizeText(it.text)
	left, contentWidth := transcriptGeometry(width)
	block := surfaceStyle().MarginLeft(left).Width(contentWidth)
	body := func(style lipgloss.Style, text string) string {
		return style.Inherit(block).Render(text)
	}
	// thread 在消息体左侧垂下一根针脚线，把消息标记与正文缝在一起。
	thread := func(style lipgloss.Style, text string) string {
		if contentWidth < 8 {
			return body(style, text)
		}
		return style.Inherit(block).
			BorderStyle(stitchBorder()).
			BorderLeft(true).
			BorderForeground(colorLine).
			Width(contentWidth - 1).
			Render(text)
	}
	header := func(marker, label string, color lipgloss.TerminalColor) string {
		return block.
			Foreground(color).
			Bold(true).
			Render(marker + " " + label)
	}

	switch it.kind {
	case itemUser:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			header("◆", "YOU", colorInk),
			thread(surfaceStyle().Foreground(colorDim), it.text),
		)
	case itemAssistant:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			header("◇", "TM", colorDim),
			thread(surfaceStyle().Foreground(colorMuted), it.text),
		)
	case itemReport:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			header("◆", "TASK REPORT", colorForeground),
			thread(surfaceStyle().Foreground(colorDim), it.text),
		)
	case itemActivity:
		marker := surfaceStyle().Foreground(colorMuted).Bold(true).Render("› ")
		text := surfaceStyle().Foreground(colorMuted).Render(it.text)
		return block.Render(marker + text)
	case itemThinking:
		return body(surfaceStyle().Foreground(colorFaint).Italic(true), "◌ "+it.text)
	case itemError:
		return body(errorStyle(), "▲ "+it.text)
	default:
		return body(surfaceStyle(), it.text)
	}
}

func sanitizeText(text string) string {
	text = ansi.Strip(text)
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < ' ' || r == '\x7f' || (r >= '\x80' && r <= '\x9f'):
			return -1
		default:
			return r
		}
	}, text)
}

func transcriptGeometry(width int) (left, contentWidth int) {
	if width <= 1 {
		return 0, 1
	}
	left = 2
	contentWidth = width - left
	if contentWidth > 86 {
		contentWidth = 86
	}
	return left, contentWidth
}

func joinEnds(left, right string, width, gap int) string {
	if right == "" || width <= 0 {
		return left
	}
	padding := width - lipgloss.Width(left) - lipgloss.Width(right)
	if padding < gap {
		return left
	}
	return left + strings.Repeat(" ", padding) + right
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
