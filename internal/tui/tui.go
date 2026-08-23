// Package tui 是 threadmill 的默认交互界面：聊天、实时 Graph、状态栏与输入框。
//
// 布局沿用「滚动 transcript + 底部 dock」，视觉为近黑蓝画布与单一电光蓝强调。
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
	kind itemKind
	text string
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
	chat      Chat
	info      Info
	viewport  viewport.Model
	input     textarea.Model
	items     []item
	mode      viewMode
	tasks     map[string]taskView
	taskOrder []string
	streamed  bool
	tokens    int
	inflight  map[string]int
	spin      spinner.Model
	width     int
	height    int
}

func newModel(chat Chat, info Info) model {
	input := textarea.New()
	input.Placeholder = "发给 manager"
	input.Focus()
	input.Prompt = "> "
	input.ShowLineNumbers = false
	input.SetHeight(1)
	input.Cursor.Style = lipgloss.NewStyle().
		Foreground(colorCanvas).
		Background(colorAccent)
	input.FocusedStyle.Base = inputBase(colorAccent)
	input.FocusedStyle.CursorLine = surfaceStyle()
	input.FocusedStyle.Placeholder = surfaceStyle().Foreground(colorFaint)
	input.FocusedStyle.Prompt = surfaceStyle().Foreground(colorAccent).Bold(true)
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

	spin := spinner.New(spinner.WithSpinner(spinner.Line))

	return model{
		chat:     chat,
		info:     info,
		viewport: vp,
		input:    input,
		tasks:    map[string]taskView{},
		inflight: map[string]int{},
		spin:     spin,
	}
}

// NewProgram 构造全屏 TUI。调用方应在 Run 前把 Program.Send 接到 manager 事件上。
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
		case tea.KeyTab:
			if m.mode == viewChat {
				m.mode = viewGraph
			} else {
				m.mode = viewChat
			}
			m.layout()
			return m, nil
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
	m.observeTask(ev)
	switch {
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseStart && ev.AgentID == "manager":
		m.items = append(m.items, item{kind: itemThinking, text: "[manager] 思考中"})
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseDelta && ev.AgentID == "manager" && ev.Delta != "":
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

func (m *model) observeTask(ev event.RuntimeEvent) {
	taskID, role := splitTaskAgent(ev.AgentID)
	if taskID == "" {
		return
	}
	task, ok := m.tasks[taskID]
	if !ok {
		task = taskView{
			id:         taskID,
			outcome:    "active",
			seenRoles:  map[string]bool{},
			failedRole: map[string]bool{},
		}
		m.taskOrder = append(m.taskOrder, taskID)
	}
	if role != "" {
		task.seenRoles[role] = true
		if ev.Err != "" {
			task.failedRole[role] = true
		}
	}
	if ev.Kind == event.KindTask && ev.Phase == event.PhaseEnd {
		task.outcome = ev.Name
		if task.outcome == "" {
			task.outcome = "done"
		}
	}
	m.tasks[taskID] = task
}

func splitTaskAgent(agentID string) (taskID, role string) {
	if !strings.HasPrefix(agentID, "task-") {
		return "", ""
	}
	taskID, role, _ = strings.Cut(agentID, ":")
	switch role {
	case "", "planner", "executor", "verifier":
		return taskID, role
	default:
		return taskID, ""
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
	if m.mode == viewChat {
		chatWidth, _, graphHeight, sideBySide := chatGraphGeometry(m.width, h)
		m.viewport.Width = chatWidth
		if !sideBySide && graphHeight > 0 {
			m.viewport.Height = max(1, h-graphHeight-1)
		}
	}
	m.refresh()
}

func (m *model) refresh() {
	width := m.viewport.Width
	if width <= 0 {
		width = 80
	}
	if m.mode == viewGraph {
		m.viewport.SetContent(m.graphView(width))
		m.viewport.GotoTop()
		return
	}
	if len(m.items) == 0 {
		left, contentWidth := transcriptGeometry(width)
		hint := surfaceStyle().
			Foreground(colorFaint).
			MarginLeft(left).
			Width(contentWidth).
			Render("SESSION READY  ·  输入任务后按 Enter")
		m.viewport.SetContent("\n" + hint)
		return
	}
	parts := make([]string, 0, len(m.items))
	for _, it := range m.items {
		parts = append(parts, renderItem(it, width))
	}
	m.viewport.SetContent("\n" + strings.Join(parts, "\n\n"))
	m.viewport.GotoBottom()
}

func (m model) graphView(width int) string {
	left, contentWidth := transcriptGeometry(width)
	header := surfaceStyle().
		Foreground(colorForeground).
		Bold(true).
		MarginLeft(left).
		Width(contentWidth).
		Render(fmt.Sprintf("GRAPH  ─  LIVE EXECUTION  ·  %d NODES", len(m.taskOrder)*3))
	if len(m.taskOrder) == 0 {
		empty := surfaceStyle().
			Foreground(colorFaint).
			MarginLeft(left).
			Width(contentWidth).
			Render("NO TASKS YET  ·  任务启动后会在这里展开")
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
		nodes[i] = renderGraphNode(task.id, role, statuses[i])
	}
	arrows := []string{
		graphEdgeStyle(statuses[1], " ──▶ ", " ┄┄▶ ", " ──▶ "),
		graphEdgeStyle(statuses[2], " ──▶ ", " ┄┄▶ ", " ──▶ "),
	}
	row := lipgloss.JoinHorizontal(
		lipgloss.Center,
		nodes[0], arrows[0], nodes[1], arrows[1], nodes[2],
	)
	if lipgloss.Width(row) > width {
		blueDown := surfaceStyle().Foreground(colorAccentDim).Render("↓")
		brightDown := surfaceStyle().Foreground(colorAccent).Render("↓")
		row = lipgloss.JoinVertical(
			lipgloss.Center,
			nodes[0],
			blueDown,
			nodes[1],
			brightDown,
			nodes[2],
		)
	}
	return lipgloss.NewStyle().MarginLeft(left).Render(row)
}

// graphEdgeStyle 按目标节点状态给连线上色：
// 目标运行中=电光蓝实线（对应设计稿 run 边），目标待命=弱文本点线，其余=常态灰。
func graphEdgeStyle(target taskNodeStatus, run, pend, done string) string {
	switch target {
	case taskActive:
		return surfaceStyle().Foreground(colorAccent).Render(run)
	case taskPending:
		return surfaceStyle().Foreground(colorFaint).Render(pend)
	default:
		return surfaceStyle().Foreground(colorMuted).Render(done)
	}
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
	return surfaceStyle().Width(m.width).Height(m.height).Render(view)
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
			tokens = append(tokens, renderMiniGraphToken(role, statuses[i]))
			if i < len(roles)-1 {
				tokens = append(tokens, graphEdgeStyle(statuses[i+1], "→", "→", "→"))
			}
		}
		chain := lipgloss.JoinHorizontal(lipgloss.Center, tokens...)
		detail := surfaceStyle().Foreground(colorFaint).Render(task.id)
		return lipgloss.JoinVertical(lipgloss.Center, chain, detail)
	}

	nodes := make([]string, 0, len(roles))
	for i, role := range roles {
		nodes = append(nodes, renderMiniGraphNode(role, statuses[i], nodeW))
	}
	arrow := func(target taskNodeStatus) string {
		return graphEdgeStyle(target, " → ", " → ", " → ")
	}
	chain := lipgloss.JoinHorizontal(
		lipgloss.Center,
		nodes[0], arrow(statuses[1]), nodes[1], arrow(statuses[2]), nodes[2],
	)
	caption := surfaceStyle().Foreground(colorFaint).Render(task.id)
	return lipgloss.JoinVertical(lipgloss.Center, chain, caption)
}

func (m model) headerLine() string {
	modelName := m.info.Model
	if modelName == "" {
		modelName = "-"
	}
	brand := lipgloss.NewStyle().
		Foreground(colorForeground).
		Background(colorCanvas).
		Bold(true).
		Render("THREADMILL")
	stripe := lipgloss.JoinHorizontal(
		lipgloss.Top,
		lipgloss.NewStyle().Foreground(colorAccent).Background(colorCanvas).Bold(true).Render("▮"),
		lipgloss.NewStyle().Foreground(colorMBlue).Background(colorCanvas).Bold(true).Render("▮"),
		lipgloss.NewStyle().Foreground(colorMRed).Background(colorCanvas).Bold(true).Render("▮"),
	)
	left := brand + " " + stripe
	right := lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorCanvas).
		Render(m.modeLabel() + "  ·  " + modelName + "  ·  " + shortenPath(m.info.Root, 40))
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
		active := lipgloss.NewStyle().
			Foreground(colorAccent).
			Background(colorCanvas).
			Bold(true).
			Render(m.spin.View() + " ACTIVE")
		left = active + lipgloss.NewStyle().
			Foreground(colorMuted).
			Background(colorCanvas).
			Render(" · "+metrics)
	}
	right := lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorCanvas).
		Render("TAB  " + m.otherModeLabel() + " · ENTER  SEND · ESC  CANCEL · ^C  QUIT")
	return lipgloss.NewStyle().
		Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderTop(true).
		BorderForeground(colorLine).
		Render(joinEnds(left, right, m.width, 2))
}

func (m model) modeLabel() string {
	if m.mode == viewGraph {
		return "GRAPH"
	}
	return "CHAT"
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
	i := strings.IndexByte(agentID, ':')
	if i <= 0 {
		return ""
	}
	return agentID[:i]
}

type taskNodeStatus int

const (
	taskPending taskNodeStatus = iota
	taskActive
	taskDone
	taskFailed
)

func (m model) taskRoleStatus(task taskView, role string) taskNodeStatus {
	if task.failedRole[role] {
		return taskFailed
	}
	if m.inflight[task.id+":"+role] > 0 {
		return taskActive
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
	if (task.outcome == "failed" || task.outcome == "canceled") && role == lastSeen {
		return taskFailed
	}
	if task.outcome == "active" && role == lastSeen {
		return taskActive
	}
	return taskDone
}

// 调色板对齐设计稿 token：近黑蓝底 + 单一电光蓝磷光强调。
var (
	colorCanvas     = lipgloss.Color("#12141B") // bg
	colorPanel      = lipgloss.Color("#161A22") // panel
	colorRaise      = lipgloss.Color("#1C212B") // raise — 节点面
	colorLine       = lipgloss.Color("#323A4A") // line — 发丝线
	colorLineHover  = lipgloss.Color("#4A5468") // line-2
	colorForeground = lipgloss.Color("#E4E9F1") // fg
	colorDim        = lipgloss.Color("#BDC6D4") // dim
	colorMuted      = lipgloss.Color("#8B96A8") // mut
	colorFaint      = lipgloss.Color("#636E80") // faint
	colorAccent     = lipgloss.Color("#7BD6FF") // acc — 电光蓝磷光
	colorAccentDim  = lipgloss.Color("#5FA9E8") // acc-2
	colorMBlue      = lipgloss.Color("#315FA8")
	colorMRed       = lipgloss.Color("#E32636")
	colorError      = lipgloss.Color("#F06C75")
)

func surfaceStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Background(colorCanvas)
}

func inputBase(line lipgloss.TerminalColor) lipgloss.Style {
	return surfaceStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(line)
}

// graphNodeVisual 给出节点状态符号、文字色、描边色。
// pending=虚线弱节点、run=电光蓝描边、done=常态面+蓝弱档符号、urg=红色。
func graphNodeVisual(status taskNodeStatus) (marker string, color, border lipgloss.TerminalColor) {
	switch status {
	case taskActive:
		return "◌", colorAccent, colorAccent
	case taskDone:
		return "✓", colorForeground, colorLine
	case taskFailed:
		return "▲", colorError, colorError
	default:
		return "○", colorMuted, colorLine
	}
}

func renderGraphNode(taskID, role string, status taskNodeStatus) string {
	marker, color, border := graphNodeVisual(status)
	label := lipgloss.NewStyle().Foreground(color).Background(colorRaise).Bold(true).
		Render(marker + " " + strings.ToUpper(role))
	detail := lipgloss.NewStyle().Foreground(colorFaint).Background(colorRaise).Render(taskID)
	return lipgloss.NewStyle().
		Foreground(colorDim).
		Background(colorRaise).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(14).
		Render(label + "\n" + detail)
}

func renderMiniGraphNode(role string, status taskNodeStatus, width int) string {
	marker, color, border := graphNodeVisual(status)
	label := lipgloss.NewStyle().
		Foreground(color).
		Background(colorRaise).
		Bold(true).
		Width(width).
		Align(lipgloss.Center).
		Render(marker + " " + strings.ToUpper(role))
	detail := lipgloss.NewStyle().
		Foreground(colorFaint).
		Background(colorRaise).
		Width(width).
		Align(lipgloss.Center).
		Render(miniStatusWord(status))
	return lipgloss.NewStyle().
		Background(colorRaise).
		BorderStyle(lipgloss.NormalBorder()).
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
	default:
		return "wait"
	}
}

func renderMiniGraphToken(role string, status taskNodeStatus) string {
	marker, color, _ := miniGraphVisual(status)
	label := "PLAN"
	switch role {
	case "executor":
		label = "EXEC"
	case "verifier":
		label = "VERIFY"
	}
	return surfaceStyle().Foreground(color).Bold(true).Render(marker + " " + label)
}

func miniGraphVisual(status taskNodeStatus) (marker string, color, border lipgloss.TerminalColor) {
	return graphNodeVisual(status)
}

func renderItem(it item, width int) string {
	left, contentWidth := transcriptGeometry(width)
	block := surfaceStyle().MarginLeft(left).Width(contentWidth)
	body := func(style lipgloss.Style, text string) string {
		return style.Inherit(block).Render(text)
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
			header("◆", "YOU", colorAccent),
			body(surfaceStyle().Foreground(colorDim), it.text),
		)
	case itemAssistant:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			header("◇", "TM", colorDim),
			body(surfaceStyle().Foreground(colorMuted), it.text),
		)
	case itemReport:
		return lipgloss.JoinVertical(
			lipgloss.Left,
			header("◆", "TASK REPORT", colorAccentDim),
			body(surfaceStyle().Foreground(colorDim), it.text),
		)
	case itemActivity:
		marker := surfaceStyle().Foreground(colorAccentDim).Bold(true).Render("› ")
		text := surfaceStyle().Foreground(colorMuted).Render(it.text)
		return block.Render(marker + text)
	case itemThinking:
		return body(surfaceStyle().Foreground(colorFaint).Italic(true), "◌ "+it.text)
	case itemError:
		return body(surfaceStyle().Foreground(colorError).Bold(true), "▲ "+it.text)
	default:
		return body(surfaceStyle(), it.text)
	}
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
