package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/KDZZZZZZ/threadmill/internal/event"
)

type fakeChat struct {
	sent     []string
	canceled int
}

func (f *fakeChat) Send(text string) { f.sent = append(f.sent, text) }
func (f *fakeChat) Cancel() bool {
	f.canceled++
	return true
}

func TestEnterSendsAndQuitCommand(t *testing.T) {
	chat := &fakeChat{}
	m := sized(newModel(chat, Info{Root: "/ws", Model: "stub"}))

	m = typeAndEnter(t, m, "hello")
	if len(chat.sent) != 1 || chat.sent[0] != "hello" {
		t.Fatalf("sent = %#v", chat.sent)
	}
	if !strings.Contains(m.viewport.View(), "hello") {
		t.Fatalf("view missing user text: %q", m.viewport.View())
	}

	m.input.SetValue("/quit")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("want quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cmd = %T", cmd())
	}
	_ = next
}

func TestEscCancels(t *testing.T) {
	chat := &fakeChat{}
	m := sized(newModel(chat, Info{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil {
		t.Fatal("esc should not quit")
	}
	if chat.canceled != 1 {
		t.Fatalf("canceled = %d", chat.canceled)
	}
}

func TestCtrlCQuits(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("want quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("cmd = %T", cmd())
	}
}

func TestManagerDeltaThenSkipsFullOutput(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "Hel",
	})
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "lo",
	})
	m = apply(t, m, OutputMsg{Text: "Hello"})
	view := m.viewport.View()
	if strings.Count(view, "Hello") != 1 {
		t.Fatalf("view = %q", view)
	}
	if strings.Contains(view, "HelHel") {
		t.Fatalf("duplicated delta: %q", view)
	}
}

func TestManagerEmptyActivityKeepsFullOutput(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.ModelDelta("manager", ""))
	m = apply(t, m, OutputMsg{Text: "completed snapshot"})
	view := m.viewport.View()
	if strings.Count(view, "completed snapshot") != 1 {
		t.Fatalf("view = %q", view)
	}
}

func TestReportAndToolActivity(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-3:executor",
		Kind:    event.KindTool,
		Phase:   event.PhaseStart,
		Name:    "bash",
	})
	m = apply(t, m, OutputMsg{Text: "[任务报告] task-3 · done · 耗时 1s\n目标: x"})
	view := m.viewport.View()
	if !strings.Contains(view, "[task-3:executor] tool bash start") {
		t.Fatalf("missing activity: %q", view)
	}
	if !strings.Contains(view, "[任务报告] task-3") {
		t.Fatalf("missing report: %q", view)
	}
}

func TestChromeShowsTitleAndKeyHints(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	view := m.View()
	if !strings.Contains(view, "THREADMILL") {
		t.Fatalf("missing title: %q", view)
	}
	if !strings.Contains(view, "ENTER  SEND") {
		t.Fatalf("missing key hints: %q", view)
	}
	if !strings.Contains(view, "deepseek") {
		t.Fatalf("missing model: %q", view)
	}
}

func TestHeaderCarriesMStripeAccent(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	if view := m.View(); !strings.Contains(view, "▮▮▮") {
		t.Fatalf("missing three-color M stripe: %q", view)
	}
}

func TestRunningViewKeepsRedInBrandOnly(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m.items = []item{{kind: itemActivity, text: "tool bash start"}}
	m.refresh()
	if got := strings.Count(m.View(), "38;2;227;38;54"); got != 1 {
		t.Fatalf("chat M red occurrences = %d, want brand stripe only", got)
	}

	m.mode = viewGraph
	m.tasks = map[string]taskView{
		"task-1": {
			id:         "task-1",
			outcome:    "active",
			seenRoles:  map[string]bool{"planner": true, "executor": true},
			failedRole: map[string]bool{},
		},
	}
	m.taskOrder = []string{"task-1"}
	m.inflight = map[string]int{"task-1:executor": 1}
	m.refresh()

	if got := strings.Count(m.View(), "38;2;227;38;54"); got != 1 {
		t.Fatalf("graph M red occurrences = %d, want brand stripe only", got)
	}
}

func TestVisualLanguageSeparatesConversationLayers(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m.items = []item{
		{kind: itemUser, text: "ship the TUI"},
		{kind: itemAssistant, text: "working on it"},
		{kind: itemActivity, text: "bash start"},
	}
	m.refresh()

	view := m.View()
	for _, want := range []string{
		"THREADMILL",
		"▮",
		"YOU",
		"TM",
		"› bash start",
		"ENTER  SEND",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %q", want, view)
		}
	}
}

func TestChatKeepsRuntimeGraphAtTopRight(t *testing.T) {
	m := newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"})
	m.items = []item{{kind: itemUser, text: "ship the TUI"}}
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-2:planner",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})

	lines := strings.Split(m.View(), "\n")
	userLine, userColumn := textPosition(lines, "YOU")
	graphLine, graphColumn := textPosition(lines, "PLANNER")
	if userLine < 0 || graphLine < 0 {
		t.Fatalf("chat or graph missing: %q", m.View())
	}
	if graphLine > 5 || graphColumn < 60 {
		t.Fatalf("graph at line %d column %d, want top-right", graphLine, graphColumn)
	}
	if userColumn >= graphColumn {
		t.Fatalf("chat column %d must stay left of graph column %d", userColumn, graphColumn)
	}
}

func TestChatMiniGraphFitsGraphColumn(t *testing.T) {
	m := newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"})
	m.tasks = map[string]taskView{
		"task-9": {id: "task-9", outcome: "active", seenRoles: map[string]bool{"planner": true, "executor": true}, failedRole: map[string]bool{}},
	}
	m.taskOrder = []string{"task-9"}
	m.inflight = map[string]int{"task-9:executor": 1}

	_, graphWidth, _, sideBySide := chatGraphGeometry(100, 24)
	if !sideBySide {
		t.Fatal("width 100 must be side-by-side")
	}
	row := renderMiniTaskGraph(m, m.tasks["task-9"], graphWidth)
	if w := lipgloss.Width(row); w > graphWidth {
		t.Fatalf("mini graph width %d overflows its %d-column slot", w, graphWidth)
	}
	for _, want := range []string{"PLANNER", "EXECUTOR", "VERIFIER", " → ", "task-9", "run", "done", "wait"} {
		if !strings.Contains(row, want) {
			t.Fatalf("mini graph missing %q: %q", want, row)
		}
	}
}

func TestNarrowChatStacksRuntimeGraphAboveTranscript(t *testing.T) {
	m := newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"})
	m.items = []item{{kind: itemUser, text: "ship the TUI"}}
	m = apply(t, m, tea.WindowSizeMsg{Width: 48, Height: 18})
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-2:planner",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})

	view := m.View()
	lines := strings.Split(view, "\n")
	graphLine, graphColumn := textPosition(lines, "PLAN")
	userLine, userColumn := textPosition(lines, "YOU")
	if graphLine < 0 || userLine < 0 {
		t.Fatalf("chat or graph missing: %q", view)
	}
	if graphLine >= userLine || graphColumn <= userColumn {
		t.Fatalf("graph (%d,%d) must stack above and right of chat (%d,%d)", graphLine, graphColumn, userLine, userColumn)
	}
	if got := lipgloss.Width(view); got != 48 {
		t.Fatalf("width = %d, want 48", got)
	}
	if got := lipgloss.Height(view); got != 18 {
		t.Fatalf("height = %d, want 18", got)
	}
}

func TestViewFitsWindow(t *testing.T) {
	tests := []struct {
		name          string
		width, height int
	}{
		{name: "standard", width: 80, height: 24},
		{name: "narrow", width: 48, height: 14},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(&fakeChat{}, Info{Root: "/tmp/very/long/project/path", Model: "deepseek"})
			m.items = []item{{kind: itemAssistant, text: strings.Repeat("响应内容 ", 24)}}
			m = apply(t, m, tea.WindowSizeMsg{Width: tt.width, Height: tt.height})

			view := m.View()
			if got := lipgloss.Width(view); got != tt.width {
				t.Fatalf("width = %d, want %d", got, tt.width)
			}
			if got := lipgloss.Height(view); got != tt.height {
				t.Fatalf("height = %d, want %d", got, tt.height)
			}
		})
	}
}

func TestTabSwitchesToRuntimeGraph(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m = apply(t, m, event.TaskStart("task-2"))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-2:planner",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})

	view := m.View()
	for _, want := range []string{"GRAPH", "task-2", "PLANNER", "EXECUTOR", "VERIFIER"} {
		if !strings.Contains(view, want) {
			t.Fatalf("graph missing %q: %q", want, view)
		}
	}
}

func TestStatusBarTokensAndTasks(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-1:planner",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-1:planner",
		Kind:    event.KindModel,
		Phase:   event.PhaseEnd,
		Name:    "ignored",
		Tokens:  9,
	})
	got := m.statusLine()
	if !strings.Contains(got, "token 9") {
		t.Fatalf("tokens missing: %q", got)
	}
	if !strings.Contains(got, "tasks 0") {
		t.Fatalf("idle tasks: %q", got)
	}

	m = apply(t, m, event.RuntimeEvent{
		AgentID: "task-2:executor",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})
	got = m.statusLine()
	if !strings.Contains(got, "tasks 1") {
		t.Fatalf("running: %q", got)
	}
}

func TestThinkingThenModelError(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws", Model: "stub"}))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})
	if !strings.Contains(m.viewport.View(), "思考中") {
		t.Fatalf("missing thinking: %q", m.viewport.View())
	}
	if !strings.Contains(m.statusLine(), "ACTIVE") {
		t.Fatalf("status missing thinking: %q", m.statusLine())
	}

	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseEnd,
		Err:     "provider API 401: invalid key",
		IsError: true,
	})
	view := m.viewport.View()
	if strings.Contains(view, "思考中") {
		t.Fatalf("thinking stayed after error: %q", view)
	}
	if !strings.Contains(view, "provider API 401: invalid key") {
		t.Fatalf("missing error: %q", view)
	}
}

func TestDeltaClearsThinking(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseStart,
	})
	m = apply(t, m, event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "Hi",
	})
	view := m.viewport.View()
	if strings.Contains(view, "思考中") {
		t.Fatalf("thinking stayed after delta: %q", view)
	}
	if !strings.Contains(view, "Hi") {
		t.Fatalf("missing delta: %q", view)
	}
}

func sized(m model) model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return next.(model)
}

func apply(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(model)
}

func typeAndEnter(t *testing.T, m model, text string) model {
	t.Helper()
	m.input.SetValue(text)
	return apply(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func textPosition(lines []string, text string) (line, column int) {
	for i, candidate := range lines {
		if column := strings.Index(candidate, text); column >= 0 {
			return i, column
		}
	}
	return -1, -1
}
