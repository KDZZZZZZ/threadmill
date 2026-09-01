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
	id         string
	nodeID     string
	task       Task
	configured chan struct{}
	children   []helpChild
	declined   bool
}

type helpChild struct {
	from string
	task Task
}

type helpState struct {
	ID       string           `json:"id"`
	CallID   string           `json:"call_id"`
	NodeID   string           `json:"node_id"`
	Reason   string           `json:"reason"`
	Units    []helpUnit       `json:"units,omitempty"`
	Children []helpChildState `json:"children,omitempty"`
	Declined bool             `json:"declined,omitempty"`
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

type helpChildState struct {
	From   string `json:"from"`
	TaskID string `json:"task_id"`
}

type helpCoordinator struct {
	graph *Graph

	mu     sync.Mutex
	notify func(string)
	runner *runner
	byID   map[string]*helpRequest
}

// HelpTools 创建 task 请求和候选 join 工具，并允许 Run 期间增加帮助分支。
func (g *Graph) HelpTools(notify func(string)) map[string]agenttool.Tool {
	help := &helpCoordinator{
		graph:  g,
		notify: notify,
		byID:   make(map[string]*helpRequest),
	}
	join := &joinCoordinator{
		graph:    g,
		sessions: make(map[string][]JoinProgress),
	}
	g.mu.Lock()
	for _, state := range g.helps {
		if req, ok := g.helpRequestLocked(state); ok {
			help.byID[state.ID] = req
		}
	}
	g.help = help
	g.join = join
	g.mu.Unlock()
	return map[string]agenttool.Tool{
		coordRequestHelpName: requestHelpTool{help: help},
		joinToolName:         joinTool{join: join},
	}
}

type requestHelpTool struct{ help *helpCoordinator }

func (t requestHelpTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordRequestHelpName,
		Description: "当前任务需要拆分时只向 manager 提交编排建议，不直接改图。调用后本 Agent 自动暂停；manager 独立审计并决定是否物化，帮助任务会 join 回当前节点，没有可行分支时继续当前任务。",
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
	nodeID := agenttool.AgentID(ctx)
	if args.Units == nil && !t.help.graph.hasHelpRequest(nodeID, call.ID) {
		return agenttool.Output{}, fmt.Errorf("%s: units are required", coordRequestHelpName)
	}
	if err := validateHelpUnits(args.Units); err != nil {
		return agenttool.Output{}, err
	}
	result, err := t.help.request(ctx, nodeID, call.ID, reason, args.Units)
	if err != nil {
		return agenttool.Output{}, err
	}
	return agenttool.Output{Content: result}, nil
}

func (g *Graph) hasHelpRequest(nodeID, callID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, state := range g.helps {
		if state.NodeID == nodeID && state.CallID == callID {
			return true
		}
	}
	return false
}

func (h *helpCoordinator) bind(runner *runner) {
	h.mu.Lock()
	h.runner = runner
	h.mu.Unlock()
}

func (h *helpCoordinator) request(
	ctx context.Context,
	nodeID, callID, reason string,
	units []helpUnit,
) (string, error) {
	h.mu.Lock()
	if h.notify == nil || h.runner == nil {
		h.mu.Unlock()
		return "", fmt.Errorf("%s: manager is unavailable", coordRequestHelpName)
	}
	runner := h.runner
	notify := h.notify
	h.mu.Unlock()
	task, ok := h.graph.taskForNode(nodeID)
	if !ok {
		return "", fmt.Errorf("%s: requester %q is not running", coordRequestHelpName, nodeID)
	}
	if runner.stores.Files != nil && !h.graph.hasHelpRequest(nodeID, callID) {
		view := runner.stores.Files.View(roleWorkspaceID(task, nodeID))
		for _, unit := range units {
			for _, input := range unit.Inputs {
				input = strings.TrimSpace(input)
				if _, err := view.Stat(input); err != nil {
					return "", fmt.Errorf(
						"%s: unit %q input %q is not available: %w",
						coordRequestHelpName,
						unit.ID,
						input,
						err,
					)
				}
			}
		}
	}

	state, req, err := h.graph.ensureHelpRequest(nodeID, callID, reason, units)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	if existing := h.byID[state.ID]; existing != nil {
		req = existing
	} else {
		h.byID[state.ID] = req
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.byID, state.ID)
		h.mu.Unlock()
	}()
	if len(state.Children) == 0 && !state.Declined {
		frontier := ""
		if len(state.Units) > 0 {
			data, err := json.Marshal(state.Units)
			if err != nil {
				return "", fmt.Errorf("%s: encode frontier: %w", coordRequestHelpName, err)
			}
			frontier = "\nFrontier: " + string(data)
		}
		legal := formatHelpSources(h.graph.legalHelpSources(nodeID), runner.hasNodeOutput)
		notify(fmt.Sprintf(
			"[拆分请求] %s\n请求节点: %s\n原因: %s%s\n合法来源: %s\n请调用 %s 的 provide_help 动作，从其他合适节点 spawn 帮助任务；帮助任务会自动 join 回请求节点。",
			state.ID,
			nodeID,
			state.Reason,
			frontier,
			legal,
			coordOrchestrateName,
		))
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-req.configured:
		if req.declined {
			return "manager 未提供帮助任务；请在当前任务中继续。", nil
		}
		result, err := runner.runHelp(ctx, req.task, req.nodeID, req.id, req.children)
		if err != nil {
			runner.fail(err)
		}
		return result, err
	}
}

func (r *runner) hasNodeOutput(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.nodeOutput[nodeID]
	return ok
}

func (g *Graph) legalHelpSources(join string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.legalHelpSourcesLocked(join)
}

func (g *Graph) legalHelpSourcesLocked(join string) []string {
	joinNode, ok := g.nodeByIDLocked(join)
	if !ok {
		return nil
	}
	root := g.treeRootLocked(joinNode.TaskID)
	tree := g.spawnedSubtreeLocked(root)
	reachable := g.reachableNodesLocked(join)
	var sources []string
	for _, task := range g.tasks {
		if _, inTree := tree[task.ID]; !inTree {
			continue
		}
		for _, node := range task.Sequence() {
			if _, blocked := reachable[node.ID]; blocked {
				continue
			}
			sources = append(sources, node.ID)
		}
	}
	return sources
}

func formatHelpSources(sources []string, ready func(string) bool) string {
	if len(sources) == 0 {
		return "无（结束本回合）"
	}
	formatted := make([]string, 0, len(sources))
	for _, source := range sources {
		if ready == nil {
			formatted = append(formatted, source)
			continue
		}
		status := "pending"
		if ready(source) {
			status = "ready"
		}
		formatted = append(formatted, fmt.Sprintf("%s (%s)", source, status))
	}
	return strings.Join(formatted, ", ")
}

func (g *Graph) taskForNode(nodeID string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	node, ok := g.nodeByIDLocked(nodeID)
	if !ok {
		return Task{}, false
	}
	task, ok := g.taskByIDLocked(node.TaskID)
	if !ok {
		return Task{}, false
	}
	return g.decorateLocked(task), true
}

// ParseHelpRequestID returns the request named by a manager help notification.
func ParseHelpRequestID(message string) (string, bool) {
	line, _, _ := strings.Cut(message, "\n")
	id, ok := strings.CutPrefix(strings.TrimSpace(line), "[拆分请求] ")
	id = strings.TrimSpace(id)
	return id, ok && id != ""
}

// DeclineHelp resumes an unconfigured help request without adding graph tasks.
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
	req := h.byID[requestID]
	if req == nil || req.children != nil || req.declined {
		return nil
	}
	if err := h.graph.markHelpDeclined(requestID, req.nodeID); err != nil {
		return err
	}
	req.declined = true
	closeHelpConfigured(req)
	return nil
}

func (h *helpCoordinator) provide(requestID string, spawns []PendingSpawn) (provideHelpResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	req := h.byID[requestID]
	if req == nil {
		return provideHelpResult{}, fmt.Errorf("%w: %q", errUnknownHelpRequest, requestID)
	}
	if req.children != nil {
		snap := h.graph.Snapshot()
		if err := h.graph.emitTaskSink(snap.Tasks); err != nil {
			return provideHelpResult{}, err
		}
		closeHelpConfigured(req)
		return provideHelpResult{
			Snapshot: snap,
			Sources:  h.sourceStatusesLocked(req.children),
		}, nil
	}
	children, snap, err := h.graph.addHelp(requestID, req.nodeID, spawns)
	if err != nil {
		return provideHelpResult{}, err
	}
	req.children = children
	if err := h.graph.emitTaskSink(snap.Tasks); err != nil {
		return provideHelpResult{}, err
	}
	closeHelpConfigured(req)
	return provideHelpResult{
		Snapshot: snap,
		Sources:  h.sourceStatusesLocked(req.children),
	}, nil
}

func (h *helpCoordinator) sourceStatusesLocked(children []helpChild) []helpSourceStatus {
	statuses := make([]helpSourceStatus, 0, len(children))
	for _, child := range children {
		statuses = append(statuses, helpSourceStatus{
			NodeID:      child.from,
			OutputReady: h.runner != nil && h.runner.hasNodeOutput(child.from),
		})
	}
	return statuses
}

func closeHelpConfigured(req *helpRequest) {
	select {
	case <-req.configured:
	default:
		close(req.configured)
	}
}

func (g *Graph) ensureHelpRequest(
	nodeID, callID, reason string,
	units []helpUnit,
) (helpState, *helpRequest, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, state := range g.helps {
		if state.NodeID == nodeID && state.CallID == callID {
			req, ok := g.helpRequestLocked(state)
			if !ok {
				break
			}
			return state, req, nil
		}
	}

	node, known := g.nodeByIDLocked(nodeID)
	task, taskKnown := g.taskByIDLocked(node.TaskID)
	if !known || !taskKnown {
		return helpState{}, nil, fmt.Errorf("%s: requester %q is not running", coordRequestHelpName, nodeID)
	}
	before := g.stateLocked()
	state := helpState{
		ID:     helpRequestID(nodeID, callID),
		CallID: callID,
		NodeID: nodeID,
		Reason: reason,
		Units:  cloneHelpUnits(units),
	}
	g.helps = append(g.helps, state)
	if err := g.saveOrRestoreLocked(before); err != nil {
		return helpState{}, nil, err
	}
	return state, &helpRequest{
		id:         state.ID,
		nodeID:     nodeID,
		task:       g.decorateLocked(task),
		configured: make(chan struct{}),
	}, nil
}

func validateHelpUnits(units []helpUnit) error {
	// Requests restored from pre-frontier checkpoints have no units. New model
	// calls cannot omit them because the public schema requires the field.
	if units == nil {
		return nil
	}
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

func (g *Graph) helpRequestLocked(state helpState) (*helpRequest, bool) {
	node, known := g.nodeByIDLocked(state.NodeID)
	task, taskKnown := g.taskByIDLocked(node.TaskID)
	if !known || !taskKnown {
		return nil, false
	}
	req := &helpRequest{
		id:         state.ID,
		nodeID:     state.NodeID,
		task:       g.decorateLocked(task),
		configured: make(chan struct{}),
		declined:   state.Declined,
	}
	if state.Declined {
		close(req.configured)
		return req, true
	}
	if len(state.Children) == 0 {
		return req, true
	}
	for _, saved := range state.Children {
		child, ok := g.taskByIDLocked(saved.TaskID)
		if !ok {
			return nil, false
		}
		req.children = append(req.children, helpChild{
			from: saved.From,
			task: g.decorateLocked(child),
		})
	}
	close(req.configured)
	return req, true
}

func (g *Graph) markHelpDeclined(requestID, nodeID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	for i := range g.helps {
		state := &g.helps[i]
		if state.ID != requestID || state.NodeID != nodeID {
			continue
		}
		if len(state.Children) > 0 || state.Declined {
			return nil
		}
		state.Declined = true
		return g.saveOrRestoreLocked(before)
	}
	return nil
}

func (g *Graph) addHelp(requestID, join string, spawns []PendingSpawn) ([]helpChild, Snapshot, error) {
	if len(spawns) == 0 {
		return nil, Snapshot{}, fmt.Errorf("%w: help spawns required", ErrInvalidPending)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	next := &Graph{
		tasks:            append([]Task(nil), g.tasks...),
		edges:            append([]Edge(nil), g.edges...),
		nextID:           g.nextID,
		helps:            cloneHelpStates(g.helps),
		statePath:        g.statePath,
		publishingTaskID: g.publishingTaskID,
		publishedTaskID:  g.publishedTaskID,
	}
	helpIndex := -1
	for i := range next.helps {
		if next.helps[i].ID == requestID && next.helps[i].NodeID == join {
			helpIndex = i
			break
		}
	}
	if helpIndex < 0 {
		return nil, Snapshot{}, fmt.Errorf("%w: %q", errUnknownHelpRequest, requestID)
	}
	legalSources := next.legalHelpSourcesLocked(join)
	legal := make(map[string]struct{}, len(legalSources))
	for _, source := range legalSources {
		legal[source] = struct{}{}
	}
	type validatedSpawn struct {
		from        string
		info        string
		parentEnvID string
	}
	validated := make([]validatedSpawn, 0, len(spawns))
	for _, spawn := range spawns {
		from := strings.TrimSpace(spawn.From)
		info := strings.TrimSpace(spawn.Info)
		if from == "" || info == "" {
			return nil, Snapshot{}, fmt.Errorf("%w: help from and info are required", ErrInvalidPending)
		}
		fromNode, ok := next.nodeByIDLocked(from)
		if !ok {
			return nil, Snapshot{}, fmt.Errorf(
				"%w: %q; 合法来源: %s",
				ErrUnknownNode,
				from,
				formatHelpSources(legalSources, nil),
			)
		}
		if _, ok := legal[from]; !ok {
			return nil, Snapshot{}, fmt.Errorf(
				"%w: %q -> %q; 合法来源: %s",
				ErrJoinCycle,
				from,
				join,
				formatHelpSources(legalSources, nil),
			)
		}
		parent, ok := next.taskByIDLocked(fromNode.TaskID)
		if !ok {
			return nil, Snapshot{}, fmt.Errorf("%w: %q", ErrUnknownNode, from)
		}
		validated = append(validated, validatedSpawn{
			from:        from,
			info:        info,
			parentEnvID: parent.Env.ID,
		})
	}
	children := make([]helpChild, 0, len(spawns))
	savedChildren := make([]helpChildState, 0, len(spawns))
	for _, spawn := range validated {
		child := next.addSpawnLocked(spawn.parentEnvID, spawn.from, join)
		next.setInfoLocked(child.ID, spawn.info)
		child, _ = next.taskByIDLocked(child.ID)
		children = append(children, helpChild{from: spawn.from, task: child})
		savedChildren = append(savedChildren, helpChildState{From: spawn.from, TaskID: child.ID})
	}
	next.helps[helpIndex].Children = savedChildren
	next.revision = g.revision + 1
	if err := next.saveLocked(); err != nil {
		return nil, Snapshot{}, err
	}
	g.tasks = next.tasks
	g.edges = next.edges
	g.nextID = next.nextID
	g.helps = next.helps
	g.revision = next.revision
	g.publishingTaskID = next.publishingTaskID
	g.publishedTaskID = next.publishedTaskID
	return children, g.snapshotLocked(), nil
}

func helpRequestID(nodeID, callID string) string {
	return "help/" + url.PathEscape(nodeID) + "/" + url.PathEscape(callID)
}

func (g *Graph) isHelpChildJoin(nodeID, taskID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, help := range g.helps {
		if help.NodeID != nodeID {
			continue
		}
		for _, child := range help.Children {
			if child.TaskID == taskID {
				return true
			}
		}
	}
	return false
}
