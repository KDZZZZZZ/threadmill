package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const coordRequestHelpName = "coordination_requestHelp"

var errUnknownHelpRequest = errors.New("coordination: unknown help request")

type helpRequest struct {
	configured chan struct{}
}

type helpState struct {
	ID         string     `json:"id"`
	CallID     string     `json:"call_id"`
	NodeID     string     `json:"node_id"`
	PauseID    string     `json:"pause_id"`
	ResumeID   string     `json:"resume_id"`
	Reason     string     `json:"reason"`
	Units      []helpUnit `json:"units"`
	TaskIDs    []string   `json:"task_ids,omitempty"`
	Configured bool       `json:"configured,omitempty"`
	Declined   bool       `json:"declined,omitempty"`
}

type helpUnit struct {
	ID              string   `json:"id"`
	Goal            string   `json:"goal"`
	AdmissionReason string   `json:"admission_reason"`
	Inputs          []string `json:"inputs"`
	Writes          []string `json:"writes"`
	DependsOn       []string `json:"depends_on"`
	Deliverable     string   `json:"deliverable"`
}

type helpCoordinator struct {
	graph *Graph

	mu     sync.Mutex
	notify func(string)
	byID   map[string]*helpRequest
}

// HelpTools exposes task help requests and input decisions. The graph routes
// each call to the requester activation; concurrent tasks share no runner binding.
func (g *Graph) HelpTools(notify func(string)) map[string]agenttool.Tool {
	g.mu.Lock()
	if g.help == nil {
		g.help = &helpCoordinator{graph: g, byID: make(map[string]*helpRequest)}
	}
	help := g.help
	g.mu.Unlock()
	if notify != nil {
		help.mu.Lock()
		help.notify = notify
		help.mu.Unlock()
	}
	return map[string]agenttool.Tool{
		coordRequestHelpName: requestHelpTool{help: help},
		inputToolName:        inputTool{graph: g},
	}
}

type requestHelpTool struct{ help *helpCoordinator }

func (t requestHelpTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordRequestHelpName,
		Description: "向 manager 提交任务拆分建议并暂停当前 Agent。系统冻结当前文件和记忆为 Pause 输入，manager 用普通 tasks/edges 编排；只有显式接入 Resume 的边会阻塞当前 Agent，未返回的帮助任务可独立运行。恢复时先处理文件，再整理记忆。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string","minLength":1},"units":{"type":"array","minItems":1,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"goal":{"type":"string","minLength":1},"admission_reason":{"type":"string","enum":["critical_path","context_offload","race"]},"inputs":{"type":"array","items":{"type":"string","minLength":1}},"writes":{"type":"array","items":{"type":"string","minLength":1}},"depends_on":{"type":"array","items":{"type":"string","minLength":1}},"deliverable":{"type":"string","minLength":1}},"required":["id","goal","admission_reason","inputs","writes","depends_on","deliverable"],"additionalProperties":false}}},"required":["reason","units"],"additionalProperties":false}`),
	}
}

func (t requestHelpTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.help == nil || t.help.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil help coordinator", coordRequestHelpName)
	}
	if strings.TrimSpace(call.ID) == "" {
		return agenttool.Output{}, fmt.Errorf("%s: call id is required", coordRequestHelpName)
	}
	var args struct {
		Reason string     `json:"reason"`
		Units  []helpUnit `json:"units"`
	}
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return agenttool.Output{}, fmt.Errorf("%s: reason is required", coordRequestHelpName)
	}
	if err := validateHelpUnits(args.Units); err != nil {
		return agenttool.Output{}, err
	}
	result, err := t.help.request(ctx, agenttool.AgentID(ctx), call.ID, reason, args.Units)
	if err != nil {
		return agenttool.Output{}, err
	}
	return agenttool.Output{Content: result}, nil
}

func (h *helpCoordinator) request(
	ctx context.Context,
	nodeID, callID, reason string,
	units []helpUnit,
) (string, error) {
	h.mu.Lock()
	notify := h.notify
	h.mu.Unlock()
	run := h.graph.runnerForNode(nodeID)
	if run == nil {
		return "", fmt.Errorf("%s: requester %q is not running", coordRequestHelpName, nodeID)
	}
	state, exists := h.graph.helpState(helpRequestID(nodeID, callID))
	if !exists {
		if notify == nil {
			return "", fmt.Errorf("%s: manager is unavailable", coordRequestHelpName)
		}
		if err := validateHelpInputs(run, nodeID, units); err != nil {
			return "", err
		}
		pause, resume, err := run.pauseHelp(nodeID, callID)
		if err != nil {
			return "", err
		}
		state = helpState{
			ID: helpRequestID(nodeID, callID), CallID: callID, NodeID: nodeID,
			PauseID: pause.ID, ResumeID: resume.ID, Reason: reason, Units: cloneHelpUnits(units),
		}
		if err := h.graph.saveHelpRequest(state); err != nil {
			return "", err
		}
	}

	h.mu.Lock()
	req := h.requestLocked(state.ID)
	if state.Configured || state.Declined {
		if !state.Declined {
			if err := h.graph.emitTaskSink(h.graph.Snapshot().Tasks); err != nil {
				h.mu.Unlock()
				return "", err
			}
		}
		closeHelpConfigured(req)
	}
	h.mu.Unlock()
	if !state.Configured && !state.Declined {
		if notify == nil {
			return "", fmt.Errorf("%s: manager is unavailable", coordRequestHelpName)
		}
		frontier, err := json.Marshal(state.Units)
		if err != nil {
			return "", fmt.Errorf("%s: encode frontier: %w", coordRequestHelpName, err)
		}
		notify(fmt.Sprintf(
			"[拆分请求] %s\n请求节点: %s\nPause: %s\nResume: %s\n原因: %s\nFrontier: %s\n请调用 %s 的 provide_help 动作提交 tasks/edges；需要等待的结果显式接入 Resume，其他任务独立运行。",
			state.ID, nodeID, state.PauseID, state.ResumeID, state.Reason, frontier, coordOrchestrateName,
		))
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-req.configured:
	}
	state, _ = h.graph.helpState(state.ID)
	result, err := run.resumeHelp(ctx, nodeID, state.ResumeID)
	if err != nil {
		return "", err
	}
	if state.Declined {
		return "manager 未提供帮助任务；请在当前任务中继续。\n" + result, nil
	}
	return result, nil
}

func validateHelpInputs(run *runner, nodeID string, units []helpUnit) error {
	if run.stores.Files == nil {
		return nil
	}
	workspace := roleWorkspaceID(run.task, nodeID)
	if run.roles.bind == nil {
		workspace = run.task.Env.ID
	}
	view := run.stores.Files.View(workspace)
	for _, unit := range units {
		for _, input := range unit.Inputs {
			if _, err := view.Stat(strings.TrimSpace(input)); err != nil {
				return fmt.Errorf("%s: unit %q input %q is not available: %w", coordRequestHelpName, unit.ID, input, err)
			}
		}
	}
	return nil
}

func (h *helpCoordinator) requestLocked(requestID string) *helpRequest {
	if request := h.byID[requestID]; request != nil {
		return request
	}
	request := &helpRequest{configured: make(chan struct{})}
	h.byID[requestID] = request
	return request
}

func (g *Graph) taskForNode(nodeID string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	node, ok := g.nodeByIDLocked(nodeID)
	if !ok {
		return Task{}, false
	}
	return g.taskByIDLocked(node.TaskID)
}

// ParseHelpRequestID returns the request named by a manager help notification.
func ParseHelpRequestID(message string) (string, bool) {
	line, _, _ := strings.Cut(message, "\n")
	id, ok := strings.CutPrefix(strings.TrimSpace(line), "[拆分请求] ")
	id = strings.TrimSpace(id)
	return id, ok && id != ""
}

// DeclineHelp resumes an unconfigured request using its frozen pause state.
func (g *Graph) DeclineHelp(requestID string) error {
	g.mu.Lock()
	help := g.help
	g.mu.Unlock()
	if help == nil {
		return nil
	}
	return help.decline(strings.TrimSpace(requestID))
}

func (h *helpCoordinator) decline(requestID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, exists := h.graph.helpState(requestID)
	if !exists || state.Configured {
		return nil
	}
	if err := h.graph.markHelpDeclined(requestID); err != nil {
		return err
	}
	closeHelpConfigured(h.requestLocked(requestID))
	return nil
}

func (h *helpCoordinator) provide(requestID string, pending PendingSubgraph) (provideHelpResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, exists := h.graph.helpState(requestID)
	if !exists {
		return provideHelpResult{}, fmt.Errorf("%w: %q", errUnknownHelpRequest, requestID)
	}
	if state.Declined {
		return provideHelpResult{}, fmt.Errorf("%s: request %q was declined", coordOrchestrateName, requestID)
	}
	if !state.Configured {
		if err := h.graph.addHelp(requestID, pending); err != nil {
			return provideHelpResult{}, err
		}
	}
	snapshot := h.graph.Snapshot()
	if err := h.graph.emitTaskSink(snapshot.Tasks); err != nil {
		return provideHelpResult{}, err
	}
	closeHelpConfigured(h.requestLocked(requestID))
	result := provideHelpResult{Snapshot: snapshot, Sources: []helpSourceStatus{}}
	for _, source := range h.graph.Incoming(state.ResumeID) {
		_, ready := h.graph.Output(source.ID)
		result.Sources = append(result.Sources, helpSourceStatus{NodeID: source.ID, OutputReady: ready})
	}
	return result, nil
}

func closeHelpConfigured(req *helpRequest) {
	select {
	case <-req.configured:
	default:
		close(req.configured)
	}
}

func (g *Graph) helpState(requestID string) (helpState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, state := range g.helps {
		if state.ID == requestID {
			return cloneHelpStates([]helpState{state})[0], true
		}
	}
	return helpState{}, false
}

func (g *Graph) saveHelpRequest(state helpState) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, saved := range g.helps {
		if saved.ID == state.ID {
			return nil
		}
	}
	before := g.stateLocked()
	if err := g.addPendingLocked(PendingSubgraph{Edges: []Edge{{From: state.PauseID, To: state.ResumeID}}}); err != nil {
		g.applyStateLocked(before)
		return err
	}
	g.helps = append(g.helps, state)
	g.revision++
	return g.saveOrRestoreLocked(before)
}

func (g *Graph) markHelpDeclined(requestID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	for i := range g.helps {
		if g.helps[i].ID == requestID {
			g.helps[i].Configured = true
			g.helps[i].Declined = true
			g.revision++
			return g.saveOrRestoreLocked(before)
		}
	}
	return nil
}

func (g *Graph) addHelp(requestID string, pending PendingSubgraph) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	index := -1
	for i := range g.helps {
		if g.helps[i].ID == requestID {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%w: %q", errUnknownHelpRequest, requestID)
	}
	before := g.stateLocked()
	known := make(map[string]struct{}, len(g.tasks))
	for _, task := range g.tasks {
		known[task.ID] = struct{}{}
	}
	if err := g.addPendingLocked(pending); err != nil {
		g.applyStateLocked(before)
		return err
	}
	for _, task := range g.tasks {
		if _, exists := known[task.ID]; !exists {
			g.helps[index].TaskIDs = append(g.helps[index].TaskIDs, task.ID)
		}
	}
	g.helps[index].Configured = true
	g.revision++
	return g.saveOrRestoreLocked(before)
}

func validateHelpUnits(units []helpUnit) error {
	if len(units) == 0 {
		return fmt.Errorf("%s: units are required", coordRequestHelpName)
	}
	seen := make(map[string]struct{}, len(units))
	for i, unit := range units {
		id := strings.TrimSpace(unit.ID)
		if id == "" || strings.TrimSpace(unit.Goal) == "" || strings.TrimSpace(unit.Deliverable) == "" {
			return fmt.Errorf(
				"%s: unit %d requires id, goal, and deliverable",
				coordRequestHelpName,
				i,
			)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("%s: duplicate unit id %q", coordRequestHelpName, id)
		}
		seen[id] = struct{}{}
		if unit.Inputs == nil || unit.Writes == nil || unit.DependsOn == nil {
			return fmt.Errorf(
				"%s: unit %q requires inputs, writes, and depends_on arrays",
				coordRequestHelpName,
				id,
			)
		}
		for _, field := range []struct {
			name   string
			values []string
		}{
			{name: "inputs", values: unit.Inputs},
			{name: "writes", values: unit.Writes},
			{name: "depends_on", values: unit.DependsOn},
		} {
			for _, value := range field.values {
				if strings.TrimSpace(value) == "" {
					return fmt.Errorf(
						"%s: unit %q has an empty %s value",
						coordRequestHelpName,
						id,
						field.name,
					)
				}
			}
		}
		switch unit.AdmissionReason {
		case "critical_path", "context_offload", "race":
		default:
			return fmt.Errorf(
				"%s: unit %q has invalid admission_reason %q",
				coordRequestHelpName,
				id,
				unit.AdmissionReason,
			)
		}
	}
	if len(units) == 1 && units[0].AdmissionReason == "critical_path" {
		return fmt.Errorf("%s: critical_path requires at least two units", coordRequestHelpName)
	}
	return nil
}

func cloneHelpUnits(units []helpUnit) []helpUnit {
	cloned := append([]helpUnit(nil), units...)
	for i := range cloned {
		cloned[i].Inputs = cloneHelpStrings(units[i].Inputs)
		cloned[i].Writes = cloneHelpStrings(units[i].Writes)
		cloned[i].DependsOn = cloneHelpStrings(units[i].DependsOn)
	}
	return cloned
}

func cloneHelpStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string{}, values...)
}

func helpRequestID(nodeID, callID string) string {
	return "help/" + url.PathEscape(nodeID) + "/" + url.PathEscape(callID)
}
