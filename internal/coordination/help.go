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

const (
	coordRequestHelpName = "coordination_requestHelp"
	coordProvideHelpName = "coordination_provideHelp"
)

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
	Children []helpChildState `json:"children,omitempty"`
	Declined bool             `json:"declined,omitempty"`
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

// HelpTools 创建 task 请求、manager 响应和候选 join 工具，并允许 Run 期间增加帮助分支。
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
		coordProvideHelpName: provideHelpTool{help: help},
		joinToolName:         joinTool{join: join},
	}
}

type requestHelpTool struct{ help *helpCoordinator }

func (t requestHelpTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordRequestHelpName,
		Description: "当前任务需要拆分时请求 manager 改图。调用后本 Agent 自动暂停；帮助任务会 join 回当前节点，没有可行分支时 manager 会结束等待并让当前任务继续。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"],"additionalProperties":false}`),
	}
}

func (t requestHelpTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.help == nil || t.help.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil help coordinator", coordRequestHelpName)
	}
	var args struct {
		Reason string `json:"reason"`
	}
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return agenttool.Output{}, fmt.Errorf("%s: reason is required", coordRequestHelpName)
	}
	result, err := t.help.request(ctx, agenttool.AgentID(ctx), call.ID, reason)
	if err != nil {
		return agenttool.Output{}, err
	}
	return agenttool.Output{Content: result}, nil
}

type provideHelpTool struct{ help *helpCoordinator }

func (t provideHelpTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordProvideHelpName,
		Description: "响应 task 的拆分请求：从其他不会成环的节点 spawn 帮助任务，并自动 join 回请求节点。一次调用提交完整帮助列表；没有合法来源时不要调用。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"request_id":{"type":"string"},"spawns":{"type":"array","minItems":1,"items":{"type":"object","properties":{"from":{"type":"string"},"info":{"type":"string"}},"required":["from","info"],"additionalProperties":false}}},"required":["request_id","spawns"],"additionalProperties":false}`),
	}
}

func (t provideHelpTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.help == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil help coordinator", coordProvideHelpName)
	}
	var args struct {
		RequestID string         `json:"request_id"`
		Spawns    []PendingSpawn `json:"spawns"`
	}
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	snap, err := t.help.provide(strings.TrimSpace(args.RequestID), args.Spawns)
	if err != nil {
		return agenttool.Output{}, err
	}
	return encodeGraphJSON(snap)
}

func (h *helpCoordinator) bind(runner *runner) {
	h.mu.Lock()
	h.runner = runner
	h.mu.Unlock()
}

func (h *helpCoordinator) request(ctx context.Context, nodeID, callID, reason string) (string, error) {
	h.mu.Lock()
	if h.notify == nil || h.runner == nil {
		h.mu.Unlock()
		return "", fmt.Errorf("%s: manager is unavailable", coordRequestHelpName)
	}
	runner := h.runner
	notify := h.notify
	h.mu.Unlock()

	state, req, err := h.graph.ensureHelpRequest(nodeID, callID, reason)
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
		notify(fmt.Sprintf(
			"[拆分请求] %s\n请求节点: %s\n原因: %s\n请调用 %s，从其他合适节点 spawn 帮助任务；帮助任务会自动 join 回请求节点。",
			state.ID,
			nodeID,
			state.Reason,
			coordProvideHelpName,
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

func (h *helpCoordinator) provide(requestID string, spawns []PendingSpawn) (Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	req := h.byID[requestID]
	if req == nil {
		return Snapshot{}, fmt.Errorf("%w: %q", errUnknownHelpRequest, requestID)
	}
	if req.children != nil {
		snap := h.graph.Snapshot()
		if err := h.graph.emitTaskSink(snap.Tasks); err != nil {
			return Snapshot{}, err
		}
		closeHelpConfigured(req)
		return snap, nil
	}
	children, snap, err := h.graph.addHelp(requestID, req.nodeID, spawns)
	if err != nil {
		return Snapshot{}, err
	}
	req.children = children
	if err := h.graph.emitTaskSink(snap.Tasks); err != nil {
		return Snapshot{}, err
	}
	closeHelpConfigured(req)
	return snap, nil
}

func closeHelpConfigured(req *helpRequest) {
	select {
	case <-req.configured:
	default:
		close(req.configured)
	}
}

func (g *Graph) ensureHelpRequest(nodeID, callID, reason string) (helpState, *helpRequest, error) {
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
		tasks:     append([]Task(nil), g.tasks...),
		edges:     append([]Edge(nil), g.edges...),
		nextID:    g.nextID,
		helps:     cloneHelpStates(g.helps),
		statePath: g.statePath,
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
	children := make([]helpChild, 0, len(spawns))
	savedChildren := make([]helpChildState, 0, len(spawns))
	for _, spawn := range spawns {
		from := strings.TrimSpace(spawn.From)
		if from == "" || strings.TrimSpace(spawn.Info) == "" {
			return nil, Snapshot{}, fmt.Errorf("%w: help from and info are required", ErrInvalidPending)
		}
		child, err := next.spawnLocked(from, join)
		if err != nil {
			return nil, Snapshot{}, err
		}
		next.setInfoLocked(child.ID, spawn.Info)
		child, _ = next.taskByIDLocked(child.ID)
		children = append(children, helpChild{from: from, task: next.decorateLocked(child)})
		savedChildren = append(savedChildren, helpChildState{From: from, TaskID: child.ID})
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
