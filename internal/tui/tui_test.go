package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
	if !strings.Contains(view, "threadmill") {
		t.Fatalf("missing title: %q", view)
	}
	if !strings.Contains(view, "enter send") {
		t.Fatalf("missing key hints: %q", view)
	}
	if !strings.Contains(view, "deepseek") {
		t.Fatalf("missing model: %q", view)
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
	if !strings.Contains(got, "/tmp/proj") || !strings.Contains(got, "deepseek") {
		t.Fatalf("status = %q", got)
	}
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
	if !strings.Contains(m.statusLine(), "思考") {
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
