package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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

func TestManagerFinalOutputCompletesPartialStream(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.ModelStart("manager", 1, 0))
	m = apply(t, m, event.ModelDelta("manager", "Hel"))
	m = apply(t, m, event.ModelEnd("manager", "stub", time.Time{}, 0, 0, 0, nil))
	m = apply(t, m, OutputMsg{Text: "Hello"})

	view := m.viewport.View()
	if strings.Count(view, "Hello") != 1 || strings.Contains(view, "HelHel") {
		t.Fatalf("view = %q", view)
	}
}

func TestFailedStreamDoesNotSuppressNextTurnOutput(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.ModelStart("manager", 1, 0))
	m = apply(t, m, event.ModelDelta("manager", "partial"))
	m = apply(t, m, event.ModelEnd("manager", "stub", time.Time{}, 0, 0, 0, errors.New("broken stream")))
	m = apply(t, m, event.ModelStart("manager", 2, 0))
	m = apply(t, m, event.ModelEnd("manager", "stub", time.Time{}, 0, 0, 0, nil))
	m = apply(t, m, OutputMsg{Text: "recovered answer"})

	if view := m.viewport.View(); !strings.Contains(view, "recovered answer") {
		t.Fatalf("next turn output was suppressed: %q", view)
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

func TestManagerDeltasCoalesceTranscriptRefreshes(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/ws"}))
	m = apply(t, m, event.ModelStart("manager", 1, 0))
	next, cmd := m.Update(event.ModelDelta("manager", "a"))
	m = next.(model)
	if cmd == nil {
		t.Fatal("first delta should schedule the next transcript frame")
	}
	if view := m.viewport.View(); !strings.Contains(view, "a") {
		t.Fatalf("first delta was not rendered immediately: %q", view)
	}

	m = apply(t, m, event.ModelDelta("manager", "b"))
	if view := m.viewport.View(); strings.Contains(view, "ab") {
		t.Fatalf("second delta rebuilt transcript before the scheduled frame: %q", view)
	}
	m = apply(t, m, cmd())
	if view := m.viewport.View(); !strings.Contains(view, "ab") {
		t.Fatalf("coalesced delta missing after frame: %q", view)
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
	if !strings.Contains(view, "ENTER SEND") {
		t.Fatalf("missing key hints: %q", view)
	}
	if !strings.Contains(view, "CHAT") || !strings.Contains(view, "GRAPH") {
		t.Fatalf("missing mode tabs: %q", view)
	}
	if !strings.Contains(view, "❯") {
		t.Fatalf("missing input prompt: %q", view)
	}
	if !strings.Contains(view, "┄") {
		t.Fatalf("missing stitch rules: %q", view)
	}
	if !strings.Contains(view, "deepseek") {
		t.Fatalf("missing model: %q", view)
	}
}

func TestHeaderCarriesMStripeAccent(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	if view := m.View(); !strings.Contains(view, "▮▮▮") {
		t.Fatalf("missing woven brand stripe: %q", view)
	}
}

func TestViewIsAchromatic(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m.items = []item{
		{kind: itemUser, text: "ship the TUI"},
		{kind: itemError, text: "boom"},
	}
	m.refresh()
	assertAchromatic(t, m.View())

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
	assertAchromatic(t, m.View())
}

// sgrTrueColor 匹配 38;2;r;g;b 与 48;2;r;g;b 形式的真彩色 SGR 序列。
var sgrTrueColor = regexp.MustCompile(`(?:38|48);2;(\d+);(\d+);(\d+)`)

// assertAchromatic 守护墨织不变量:界面里的每个真彩色都必须 R==G==B。
func assertAchromatic(t *testing.T, view string) {
	t.Helper()
	matches := sgrTrueColor.FindAllStringSubmatch(view, -1)
	if len(matches) == 0 {
		t.Fatal("view carries no true color at all")
	}
	for _, match := range matches {
		if match[1] != match[2] || match[2] != match[3] {
			t.Fatalf("non-achromatic color %s in view", match[0])
		}
	}
}

func TestErrorItemsUseReverseNotHue(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	m := sized(newModel(&fakeChat{}, Info{}))
	m.items = []item{{kind: itemError, text: "boom"}}
	m.refresh()
	view := m.View()
	if !strings.Contains(view, "48;2;245;245;245") {
		t.Fatalf("error should be an ink reverse bar: %q", view)
	}
	assertAchromatic(t, view)
}

func TestEmptyTranscriptShowsWeaveMotif(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	if view := m.viewport.View(); !strings.Contains(view, "▚") {
		t.Fatalf("empty state missing weave motif: %q", view)
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
		"┆",
		"› bash start",
		"ENTER SEND",
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
	for m.weave > 0 {
		m = apply(t, m, weaveTickMsg{})
	}

	view := m.View()
	for _, want := range []string{"GRAPH", "task-2", "PLANNER", "EXECUTOR", "VERIFIER"} {
		if !strings.Contains(view, want) {
			t.Fatalf("graph missing %q: %q", want, view)
		}
	}
}

func TestTabWeaveTransition(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{Root: "/tmp/proj", Model: "deepseek"}))
	m.items = []item{{kind: itemUser, text: "ship the TUI"}}
	m.refresh()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.weave == 0 || cmd == nil {
		t.Fatalf("tab should start the weave transition, weave=%d", m.weave)
	}
	masked := ansi.Strip(m.View())
	m.weave = 0
	if visibleRunes(masked) >= visibleRunes(ansi.Strip(m.View())) {
		t.Fatal("first weave frame should hide every other row")
	}

	// resize 立即结束转场,避免遮罩错帧。
	m = sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m = apply(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.weave != 0 {
		t.Fatalf("resize should cancel the weave, got %d", m.weave)
	}
}

func visibleRunes(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' && r != '\n' {
			n++
		}
	}
	return n
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

func TestTaskLifecycleCountsBareTaskID(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.TaskStart("task-12"))
	if got := m.statusLine(); !strings.Contains(got, "tasks 1") {
		t.Fatalf("task start not counted: %q", got)
	}
	m = apply(t, m, event.TaskEnd("task-12", "done", time.Time{}, nil))
	if got := m.statusLine(); !strings.Contains(got, "tasks 0") {
		t.Fatalf("task end still counted: %q", got)
	}
}

func TestRecoveredRoleDoesNotStayFailed(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.ToolStart("task-3:executor", "bash", "call-1"))
	m = apply(t, m, event.ToolEnd(
		"task-3:executor", "bash", "call-1", time.Time{}, true, errors.New("exit 1"),
	))
	if got := m.taskRoleStatus(m.tasks["task-3"], "executor"); got != taskFailed {
		t.Fatalf("failed status = %v", got)
	}
	m = apply(t, m, event.ModelStart("task-3:executor", 1, 1))
	m = apply(t, m, event.ModelEnd("task-3:executor", "stub", time.Time{}, 0, 1, 0, nil))
	if got := m.taskRoleStatus(m.tasks["task-3"], "executor"); got == taskFailed {
		t.Fatalf("successful recovery remained failed: %v", got)
	}
}

func TestViewportScrollsAndDoesNotJumpOnNewOutput(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 40 {
		m.items = append(m.items, item{kind: itemUser, text: fmt.Sprintf("line %02d", i)})
	}
	m.refresh()
	bottom := m.viewport.YOffset
	if bottom == 0 {
		t.Fatal("test transcript did not overflow viewport")
	}

	m = apply(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.viewport.YOffset >= bottom {
		t.Fatalf("page up offset = %d, bottom = %d", m.viewport.YOffset, bottom)
	}
	scrolled := m.viewport.YOffset
	m = apply(t, m, OutputMsg{Text: "new tail"})
	if m.viewport.YOffset != scrolled {
		t.Fatalf("new output jumped from %d to %d", scrolled, m.viewport.YOffset)
	}
}

func TestViewportMouseWheelScrolls(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 40 {
		m.items = append(m.items, item{kind: itemUser, text: fmt.Sprintf("line %02d", i)})
	}
	m.refresh()
	bottom := m.viewport.YOffset
	m = apply(t, m, tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.viewport.YOffset >= bottom {
		t.Fatalf("mouse wheel offset = %d, bottom = %d", m.viewport.YOffset, bottom)
	}
}

func TestGraphViewportScrollsAndFollowsOnlyFromBottom(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 20 {
		m = apply(t, m, event.TaskStart(fmt.Sprintf("task-%d", i)))
	}
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.viewport.YOffset != 0 {
		t.Fatalf("graph opened at offset %d, want top", m.viewport.YOffset)
	}
	m = apply(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.viewport.YOffset == 0 {
		t.Fatal("graph page down did not scroll")
	}

	m.viewport.GotoBottom()
	bottom := m.viewport.YOffset
	m = apply(t, m, event.TaskStart("task-20"))
	if m.viewport.YOffset <= bottom {
		t.Fatalf("graph did not follow new task from bottom: before=%d after=%d", bottom, m.viewport.YOffset)
	}

	m.viewport.PageUp()
	scrolled := m.viewport.YOffset
	m = apply(t, m, event.TaskStart("task-21"))
	if m.viewport.YOffset != scrolled {
		t.Fatalf("graph jumped while reading history: before=%d after=%d", scrolled, m.viewport.YOffset)
	}
}

func TestSpinnerTickDoesNotMoveScrolledTranscript(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 40 {
		m.items = append(m.items, item{kind: itemUser, text: fmt.Sprintf("line %02d", i)})
	}
	m.refresh()
	m.viewport.PageUp()
	m.inflight["manager"] = 1
	want := m.viewport.YOffset
	m = apply(t, m, spinner.TickMsg{})
	if m.viewport.YOffset != want {
		t.Fatalf("spinner moved transcript from %d to %d", want, m.viewport.YOffset)
	}
}

func TestRuntimeActivitiesExplainSilentPeriods(t *testing.T) {
	tests := []struct {
		name string
		ev   event.RuntimeEvent
		want string
	}{
		{name: "manager model", ev: event.ModelStart("manager", 1, 1), want: "THINKING"},
		{name: "task model", ev: event.ModelStart("task-1:planner", 1, 1), want: "MODEL task-1:planner"},
		{name: "tool", ev: event.ToolStart("task-1:executor", "bash", "call-1"), want: "TOOL bash"},
		{name: "task", ev: event.TaskStart("task-1"), want: "TASK task-1"},
		{name: "memory", ev: event.MemoryStart("manager", "organize_subgraph", "sg-1"), want: "MEMORY organize_subgraph"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := sized(newModel(&fakeChat{}, Info{}))
			m = apply(t, m, tt.ev)
			if got := m.statusLine(); !strings.Contains(got, tt.want) {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMemoryEndRevealsOuterTaskAndMemoryErrors(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.TaskStart("task-4"))
	m = apply(t, m, event.MemoryStart("task-4:planner", "memory_search", "hidden-memory_search"))
	m = apply(t, m, event.MemoryEnd(
		"task-4:planner", "memory_search", "hidden-memory_search", time.Time{}, errors.New("memory unavailable"),
	))
	if got := m.statusLine(); !strings.Contains(got, "TASK task-4") {
		t.Fatalf("outer activity missing after memory end: %q", got)
	}
	if view := m.viewport.View(); !strings.Contains(view, "memory unavailable") {
		t.Fatalf("memory error invisible: %q", view)
	}
}

func TestTaskTerminalFailureIsVisibleWithoutReport(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.TaskStart("task-6"))
	m = apply(t, m, event.TaskEnd("task-6", "failed", time.Time{}, errors.New("workspace publish failed")))
	if view := m.viewport.View(); !strings.Contains(view, "task-6") || !strings.Contains(view, "workspace publish failed") {
		t.Fatalf("task terminal error invisible: %q", view)
	}
}

func TestTaskCancellationIsVisibleWithoutFailureStyling(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.TaskStart("task-7"))
	m = apply(t, m, event.ModelStart("task-7:planner", 1, 0))
	m = apply(t, m, event.ModelEnd("task-7:planner", "stub", time.Time{}, 0, 1, 0, nil))
	m = apply(t, m, event.TaskEnd("task-7", "canceled", time.Time{}, context.Canceled))
	if len(m.items) == 0 || m.items[len(m.items)-1].kind != itemActivity {
		t.Fatalf("cancellation item = %#v, want activity", m.items)
	}
	if got := m.taskRoleStatus(m.tasks["task-7"], "planner"); got != taskCanceled {
		t.Fatalf("canceled role status = %v", got)
	}
	if view := m.viewport.View(); !strings.Contains(view, "task-7 canceled") {
		t.Fatalf("task cancellation invisible: %q", view)
	}
}

func TestNestedMemoryLifecycleAlwaysHasNamedActivity(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	sequence := []event.RuntimeEvent{
		event.MemoryStart("subgraph-organizer", "organize_subgraph", "sg-8"),
		event.ModelStart("subgraph-organizer", 2, 1),
		event.ModelDelta("subgraph-organizer", ""),
		event.ModelEnd("subgraph-organizer", "stub", time.Time{}, 0, 3, 0, nil),
	}
	for i, ev := range sequence {
		m = apply(t, m, ev)
		if !m.busy() || m.currentActivity() == "ACTIVE" {
			t.Fatalf("step %d has anonymous or idle activity: busy=%v activity=%q", i, m.busy(), m.currentActivity())
		}
	}
	if got := m.currentActivity(); got != "MEMORY organize_subgraph" {
		t.Fatalf("activity after organizer model = %q", got)
	}
	m = apply(t, m, event.MemoryOrganized(
		"subgraph-organizer", "organize_subgraph", "sg-8", time.Time{}, 4, 2, nil,
	))
	if m.busy() || len(m.activity) != 0 {
		t.Fatalf("completed memory activity leaked: busy=%v activity=%#v", m.busy(), m.activity)
	}
}

func TestModelRetryIsVisible(t *testing.T) {
	m := sized(newModel(&fakeChat{}, Info{}))
	m = apply(t, m, event.ModelStart("manager", 1, 1))
	m = apply(t, m, event.ModelRetry("manager", 2, "rate limited"))
	if view := m.viewport.View(); !strings.Contains(view, "retry 2") || !strings.Contains(view, "rate limited") {
		t.Fatalf("retry invisible: %q", view)
	}
}

func TestSanitizeTextRemovesTerminalControlSequences(t *testing.T) {
	input := "safe\x1b[31mred\x1b[0m\x1b]52;c;YXR0YWNr\x07\r\x9b32m tail"
	got := sanitizeText(input)
	if strings.ContainsAny(got, "\x1b\x07\r\x9b") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "safered") || !strings.Contains(got, "tail") {
		t.Fatalf("printable text lost: %q", got)
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

func BenchmarkTaskDeltaUpdate(b *testing.B) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 1000 {
		m.items = append(m.items, item{kind: itemUser, text: fmt.Sprintf("transcript line %04d", i)})
	}
	m.refresh()
	ev := event.ModelDelta("task-1:executor", "hidden task delta")

	b.ReportAllocs()
	for b.Loop() {
		next, _ := m.Update(ev)
		m = next.(model)
	}
}

func BenchmarkManagerDeltaUpdate(b *testing.B) {
	m := sized(newModel(&fakeChat{}, Info{}))
	for i := range 1000 {
		m.items = append(m.items, item{kind: itemUser, text: fmt.Sprintf("transcript line %04d", i)})
	}
	m.refresh()

	b.ReportAllocs()
	for b.Loop() {
		next, _ := m.Update(event.ModelDelta("manager", "x"))
		m = next.(model)
	}
}

func TestActiveEdgeFlowsWhileBusy(t *testing.T) {
	frames := [2]string{" ─╌▶ ", " ╌─▶ "}
	first := flowEdge(0, taskActive, frames, " ┄┄▶ ", " ──▶ ")
	second := flowEdge(1, taskActive, frames, " ┄┄▶ ", " ──▶ ")
	if first == second {
		t.Fatalf("active edge should flow between frames: %q", first)
	}
	if lipgloss.Width(first) != lipgloss.Width(second) {
		t.Fatalf(
			"flow frames changed width: %d vs %d",
			lipgloss.Width(first),
			lipgloss.Width(second),
		)
	}

	m := sized(newModel(&fakeChat{}, Info{}))
	m.inflight["task-1:executor"] = 1
	m = apply(t, m, spinner.TickMsg{})
	if m.phase != 1 {
		t.Fatalf("busy tick should advance phase, got %d", m.phase)
	}
	m.inflight = map[string]int{}
	m = apply(t, m, spinner.TickMsg{})
	if m.phase != 1 {
		t.Fatalf("idle tick must freeze phase, got %d", m.phase)
	}

	// GRAPH 视图随相位重渲染:同一份任务图,相邻帧的连线不同。
	g := sized(newModel(&fakeChat{}, Info{}))
	g.mode = viewGraph
	g.tasks = map[string]taskView{
		"task-1": {
			id:         "task-1",
			outcome:    "active",
			seenRoles:  map[string]bool{"planner": true, "executor": true},
			failedRole: map[string]bool{},
		},
	}
	g.taskOrder = []string{"task-1"}
	g.inflight = map[string]int{"task-1:executor": 1}
	g.refresh()
	frame0 := g.viewport.View()
	g = apply(t, g, spinner.TickMsg{})
	if g.viewport.View() == frame0 {
		t.Fatal("graph view did not re-render on tick while busy")
	}
}

func TestActiveNodeBreathes(t *testing.T) {
	seen := map[lipgloss.TerminalColor]bool{}
	for phase := uint64(0); phase < 6; phase++ {
		seen[breatheColor(phase)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("active node border should breathe across shades, got %v", seen)
	}
	for shade := range seen {
		if shade != lipgloss.TerminalColor(colorLine) &&
			shade != lipgloss.TerminalColor(colorLineHi) &&
			shade != lipgloss.TerminalColor(colorInk) {
			t.Fatalf("breathe stepped outside the gray ramp: %v", shade)
		}
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
