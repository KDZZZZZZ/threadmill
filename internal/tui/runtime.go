package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/KDZZZZZZ/threadmill/internal/event"
)

type runtimeActivity struct {
	label string
	seq   uint64
}

type transcriptFrameMsg struct{}

// weaveTickMsg 是织入转场的帧节拍;weave 递减到 0 时转场结束。
type weaveTickMsg struct{}

const transcriptFrameInterval = time.Second / 60

// weaveFrameInterval 织入帧间隔:两帧遮罩,约 60ms 完成一次织入。
const weaveFrameInterval = 30 * time.Millisecond

// weaveFrames Tab 转场的遮罩帧数:奇数行先织,偶数行后织,随后完整呈现。
const weaveFrames = 2

func managerTextDelta(ev event.RuntimeEvent) bool {
	return ev.Kind == event.KindModel && ev.Phase == event.PhaseDelta &&
		ev.AgentID == "manager" && ev.Delta != ""
}

func nextTranscriptFrame() tea.Cmd {
	return tea.Tick(transcriptFrameInterval, func(time.Time) tea.Msg {
		return transcriptFrameMsg{}
	})
}

func nextWeaveFrame() tea.Cmd {
	return tea.Tick(weaveFrameInterval, func(time.Time) tea.Msg {
		return weaveTickMsg{}
	})
}

func (m *model) handleRuntimeEvent(ev event.RuntimeEvent) tea.Cmd {
	wasBusy := m.busy()
	changed := m.applyEvent(ev)
	taskID, _ := splitTaskAgent(ev.AgentID)
	var cmd tea.Cmd
	if changed && managerTextDelta(ev) {
		if m.mode == viewChat && !m.framePending {
			m.refresh()
			m.framePending = true
			cmd = nextTranscriptFrame()
		} else {
			m.transcriptDirty = true
		}
	} else if changed || (m.mode == viewGraph && taskID != "") {
		m.refresh()
		m.transcriptDirty = false
	}
	if m.busy() && !wasBusy {
		return tea.Batch(cmd, m.spin.Tick)
	}
	return cmd
}

func (m *model) applyEvent(ev event.RuntimeEvent) bool {
	changed := false
	m.observeTask(ev)
	m.observeActivity(ev)
	switch {
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseStart && ev.AgentID == "manager":
		m.streamed = nil
		m.items = append(m.items, item{kind: itemThinking, text: "[manager] 思考中"})
		m.thinking = true
		changed = true
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseDelta && ev.AgentID == "manager" && ev.Delta != "":
		delta := sanitizeText(ev.Delta)
		changed = m.dropThinking()
		if delta != "" {
			m.appendAssistant(delta)
			m.streamed = append(m.streamed, delta)
			changed = true
		}
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseRetry:
		line := fmt.Sprintf("[%s] model retry %d", eventAgent(ev), ev.Retries)
		if ev.RetryReason != "" {
			line += ": " + ev.RetryReason
		}
		m.items = append(m.items, item{kind: itemActivity, text: line})
		changed = true
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && (ev.Err != "" || ev.IsError):
		who := ev.AgentID
		if who == "" {
			who = "model"
		}
		errText := ev.Err
		if errText == "" {
			errText = "model failed"
		}
		m.dropThinking()
		if ev.AgentID == "manager" {
			m.streamed = nil
		}
		m.items = append(m.items, item{kind: itemError, text: fmt.Sprintf("[%s] %s", who, errText)})
		changed = true
	case ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.AgentID == "manager":
		changed = m.dropThinking()
	case ev.Kind == event.KindTool:
		line := ev.Name + " " + string(ev.Phase)
		if ev.AgentID != "" {
			line = fmt.Sprintf("[%s] tool %s %s", ev.AgentID, ev.Name, ev.Phase)
		} else {
			line = fmt.Sprintf("tool %s", line)
		}
		if ev.Err != "" || ev.IsError {
			if ev.Err != "" {
				line += ": " + ev.Err
			} else {
				line += ": failed"
			}
			m.items = append(m.items, item{kind: itemError, text: line})
			changed = true
			break
		}
		m.items = append(m.items, item{kind: itemActivity, text: line})
		changed = true
	case ev.Kind == event.KindTask && ev.Phase == event.PhaseEnd && ev.Name == "canceled":
		m.items = append(m.items, item{kind: itemActivity, text: eventAgent(ev) + " canceled"})
		changed = true
	case ev.Kind == event.KindTask && ev.Phase == event.PhaseEnd && (ev.Err != "" || ev.Name == "failed"):
		errText := ev.Err
		if errText == "" {
			errText = "task failed"
		}
		line := fmt.Sprintf("[%s] task failed: %s", eventAgent(ev), errText)
		m.items = append(m.items, item{kind: itemError, text: line})
		changed = true
	case ev.Kind == event.KindMemory && ev.Phase == event.PhaseEnd && (ev.Err != "" || ev.IsError):
		errText := ev.Err
		if errText == "" {
			errText = "failed"
		}
		line := fmt.Sprintf("[%s] memory %s: %s", eventAgent(ev), ev.Name, errText)
		m.items = append(m.items, item{kind: itemError, text: line})
		changed = true
	}
	if ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Name != "" && m.info.Model == "" {
		m.info.Model = sanitizeText(ev.Name)
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
	return changed
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
		if ev.Phase == event.PhaseEnd {
			if ev.Err != "" || ev.IsError {
				task.failedRole[role] = true
			} else {
				delete(task.failedRole, role)
			}
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

func (m *model) observeActivity(ev event.RuntimeEvent) {
	key := activityKey(ev)
	switch ev.Phase {
	case event.PhaseStart:
		m.activityN++
		m.activity[key] = runtimeActivity{label: activityLabel(ev), seq: m.activityN}
	case event.PhaseEnd:
		delete(m.activity, key)
	}
}

func activityKey(ev event.RuntimeEvent) string {
	switch ev.Kind {
	case event.KindModel, event.KindTask:
		return string(ev.Kind) + "\x00" + ev.AgentID
	case event.KindTool, event.KindMemory:
		id := ev.CallID
		if id == "" {
			id = ev.Name
		}
		return string(ev.Kind) + "\x00" + ev.AgentID + "\x00" + id
	default:
		return string(ev.Kind) + "\x00" + ev.AgentID
	}
}

func activityLabel(ev event.RuntimeEvent) string {
	agentID := sanitizeText(ev.AgentID)
	name := sanitizeText(ev.Name)
	switch ev.Kind {
	case event.KindModel:
		if ev.AgentID == "manager" {
			return "THINKING"
		}
		return "MODEL " + agentID
	case event.KindTool:
		return "TOOL " + name
	case event.KindTask:
		return "TASK " + agentID
	case event.KindMemory:
		return "MEMORY " + name
	default:
		return "ACTIVE"
	}
}

func eventAgent(ev event.RuntimeEvent) string {
	if ev.AgentID == "" {
		return string(ev.Kind)
	}
	return sanitizeText(ev.AgentID)
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
		if m.items[n-1].text != "" {
			m.items[n-1].parts = append(m.items[n-1].parts, m.items[n-1].text)
			m.items[n-1].text = ""
		}
		m.items[n-1].parts = append(m.items[n-1].parts, delta)
		return
	}
	m.items = append(m.items, item{kind: itemAssistant, parts: []string{delta}})
}

func (m *model) applyOutput(text string) bool {
	text = sanitizeText(text)
	if strings.HasPrefix(text, "[任务报告]") {
		m.items = append(m.items, item{kind: itemReport, text: text})
		return true
	}
	if text == "" {
		return false
	}
	if len(m.streamed) > 0 {
		streamed := strings.Join(m.streamed, "")
		m.streamed = nil
		if text == streamed {
			return false
		}
		if strings.HasPrefix(text, streamed) {
			m.appendAssistant(strings.TrimPrefix(text, streamed))
			return true
		}
	}
	m.appendAssistant(text)
	return true
}

func (m *model) dropThinking() bool {
	if !m.thinking {
		return false
	}
	m.thinking = false
	items := m.items
	keep := items[:0]
	for _, it := range items {
		if it.kind != itemThinking {
			keep = append(keep, it)
		}
	}
	dropped := len(keep) != len(items)
	clear(items[len(keep):])
	m.items = keep
	return dropped
}

func (m model) busy() bool {
	for _, n := range m.inflight {
		if n > 0 {
			return true
		}
	}
	return false
}

func (m model) currentActivity() string {
	latest := runtimeActivity{label: "ACTIVE"}
	for _, activity := range m.activity {
		if activity.seq > latest.seq {
			latest = activity
		}
	}
	return latest.label
}
