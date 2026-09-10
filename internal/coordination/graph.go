// Package coordination manages tasks and their ordinary directed dependencies.
package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const (
	RolePlanner  = "planner"
	RoleExecutor = "executor"
	RoleVerifier = "verifier"
)

const (
	OutcomeActive    = "active"
	OutcomeDone      = "done"
	OutcomeCanceled  = "canceled"
	OutcomeFailed    = "failed"
	OutcomeIdle      = "idle"
	OutcomeClosed    = "closed"
	RunPolicyEnabled = "enabled"
	RunPolicyHeld    = "held"
)

var (
	ErrUnknownNode = errors.New("coordination: unknown node")
	ErrCycle       = errors.New("coordination: dependency cycle")
	ErrGraphBusy   = errors.New("coordination: task input already started")
)

// New returns an empty coordination graph.
func New() *Graph {
	return &Graph{
		tasks:   []Task{},
		nodes:   []Node{},
		edges:   []Edge{},
		outputs: make(map[string]Output),
		runners: make(map[string]*runner),
		changed: make(chan struct{}),
	}
}

// Node identifies a role in one activation, or a checkpoint of that role.
type Node struct {
	ID     string
	TaskID string
	Role   string
}

// Env identifies the independent mutable state of one task activation.
type Env struct{ ID string }

// Task keeps its identity across activations; its three role nodes describe the current activation.
type Task struct {
	ID         string
	Info       string
	Env        Env
	Planner    Node
	Executor   Node
	Verifier   Node
	Outcome    string
	RunPolicy  string
	Persistent bool
	Activation uint64
}

// Sequence returns planner, executor and verifier in execution order.
func (t Task) Sequence() []Node { return []Node{t.Planner, t.Executor, t.Verifier} }

// Edge makes To consume the immutable complete output of From.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Output pairs the immutable files and memory produced by one node.
type Output struct {
	Node      Node
	FilesRef  string
	MemoryRef string
	Report    string
}

// TaskSink atomically receives the complete task-info projection.
type TaskSink func([]Task) error

// Graph is safe for concurrent callers. Each running activation has its own runner.
type Graph struct {
	mu         sync.Mutex
	tasks      []Task
	nodes      []Node
	edges      []Edge
	outputs    map[string]Output
	nextID     uint64
	helps      []helpState
	progress   ProgressStore
	help       *helpCoordinator
	taskSink   TaskSink
	statePath  string
	runners    map[string]*runner
	changed    chan struct{}
	runContext context.Context
	runWG      sync.WaitGroup
	revision   int64
	publishing publicationState
	published  publicationState
	// Publication serializes display updates without blocking task execution.
	publishMu sync.Mutex
}

// SetProgressStore sets the durable progress store for task activations.
func (g *Graph) SetProgressStore(store ProgressStore) {
	g.mu.Lock()
	g.progress = store
	g.mu.Unlock()
}

// SetTaskSink 注册 task info 的唯一投影入口，并立即提交已有 task。
func (g *Graph) SetTaskSink(sink TaskSink) error {
	if g == nil {
		return fmt.Errorf("coordination: nil graph")
	}
	g.mu.Lock()
	g.taskSink = sink
	tasks := g.snapshotLocked().Tasks
	g.mu.Unlock()
	return emitTasks(sink, tasks)
}

func (g *Graph) emitTaskSink(tasks []Task) error {
	g.mu.Lock()
	sink := g.taskSink
	g.mu.Unlock()
	return emitTasks(sink, tasks)
}

func emitTasks(sink TaskSink, tasks []Task) error {
	if sink == nil || len(tasks) == 0 {
		return nil
	}
	return sink(tasks)
}

// AddTask appends an independent task with its planner → executor → verifier edges.
func (g *Graph) AddTask() Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	task := g.addTaskLocked("")
	g.revision++
	return task
}

// Snapshot is a detached copy of tasks, dependencies and immutable history.
type Snapshot struct {
	Revision         int64    `json:"revision"`
	Executing        bool     `json:"executing"`
	PublishingTaskID string   `json:"publishing_task_id,omitempty"`
	PublishedTaskID  string   `json:"published_task_id,omitempty"`
	PublishingNodeID string   `json:"publishing_node_id,omitempty"`
	PublishedNodeID  string   `json:"published_node_id,omitempty"`
	Tasks            []Task   `json:"tasks"`
	Nodes            []Node   `json:"nodes"`
	Edges            []Edge   `json:"edges"`
	Outputs          []Output `json:"outputs"`
}

// PromptProjection omits request-varying revision and execution flags.
func (s Snapshot) PromptProjection() ([]byte, error) {
	return json.Marshal(struct {
		PublishingTaskID string   `json:"publishing_task_id,omitempty"`
		PublishedTaskID  string   `json:"published_task_id,omitempty"`
		PublishingNodeID string   `json:"publishing_node_id,omitempty"`
		PublishedNodeID  string   `json:"published_node_id,omitempty"`
		Tasks            []Task   `json:"tasks"`
		Nodes            []Node   `json:"nodes"`
		Edges            []Edge   `json:"edges"`
		Outputs          []Output `json:"outputs"`
	}{
		PublishingTaskID: s.PublishingTaskID,
		PublishedTaskID:  s.PublishedTaskID,
		PublishingNodeID: s.PublishingNodeID,
		PublishedNodeID:  s.PublishedNodeID,
		Tasks:            s.Tasks,
		Nodes:            s.Nodes,
		Edges:            s.Edges,
		Outputs:          s.Outputs,
	})
}

// Snapshot returns the current graph and all historical checkpoints.
func (g *Graph) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked()
}

func (g *Graph) snapshotLocked() Snapshot {
	return Snapshot{
		Revision:         g.revision,
		Executing:        len(g.runners) > 0,
		PublishingTaskID: g.publishing.TaskID,
		PublishedTaskID:  g.published.TaskID,
		PublishingNodeID: g.publishing.NodeID,
		PublishedNodeID:  g.published.NodeID,
		Tasks:            append([]Task{}, g.tasks...),
		Nodes:            append([]Node{}, g.nodes...),
		Edges:            append([]Edge{}, g.edges...),
		Outputs:          g.outputListLocked(),
	}
}

func (g *Graph) outputListLocked() []Output {
	outputs := make([]Output, 0, len(g.outputs))
	for _, node := range g.nodes {
		if output, ok := g.outputs[node.ID]; ok {
			outputs = append(outputs, output)
		}
	}
	return outputs
}

// Task looks up a task by its stable identity.
func (g *Graph) Task(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.taskByIDLocked(id)
}

func (g *Graph) nodeByID(id string) (Node, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.nodeByIDLocked(id)
}

// Incoming 返回指向该节点的上游角色节点。
// 按边的原有顺序且按 ID 去重；节点不存在时返回空切片。
func (g *Graph) Incoming(nodeID string) []Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	nodes := make([]Node, 0)
	if nodeID == "" {
		return nodes
	}
	if _, ok := g.nodeByIDLocked(nodeID); !ok {
		return nodes
	}

	seen := make(map[string]struct{})
	for _, edge := range g.edges {
		if edge.To != nodeID || edge.From == "" {
			continue
		}
		if _, dup := seen[edge.From]; dup {
			continue
		}
		node, ok := g.nodeByIDLocked(edge.From)
		if !ok {
			continue
		}
		seen[edge.From] = struct{}{}
		nodes = append(nodes, node)
	}
	return nodes
}

func (g *Graph) addTaskLocked(id string) Task {
	if id == "" {
		for {
			g.nextID++
			id = fmt.Sprintf("task-%d", g.nextID)
			if _, exists := g.taskByIDLocked(id); !exists {
				break
			}
		}
	} else if raw, ok := strings.CutPrefix(id, "task-"); ok {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			g.nextID = max(g.nextID, n)
		}
	}
	task := Task{ID: id, Outcome: OutcomeActive, RunPolicy: RunPolicyEnabled}
	g.addActivationLocked(&task)
	g.tasks = append(g.tasks, task)
	return task
}

func (g *Graph) addActivationLocked(task *Task) {
	task.Activation++
	task.Env = Env{ID: fmt.Sprintf("%s-%d", task.ID, task.Activation)}
	node := func(role string) Node {
		return Node{ID: fmt.Sprintf("%s:%d:%s", task.ID, task.Activation, role), TaskID: task.ID, Role: role}
	}
	task.Planner = node(RolePlanner)
	task.Executor = node(RoleExecutor)
	task.Verifier = node(RoleVerifier)
	g.nodes = append(g.nodes, task.Sequence()...)
	g.edges = append(g.edges, Edge{From: task.Planner.ID, To: task.Executor.ID}, Edge{From: task.Executor.ID, To: task.Verifier.ID})
}

// ponytail: ordered slice lookups; add ID indexes if graph edits become a bottleneck.
func (g *Graph) taskByIDLocked(id string) (Task, bool) {
	for _, task := range g.tasks {
		if task.ID == id {
			return task, true
		}
	}
	return Task{}, false
}

func (g *Graph) nodeByIDLocked(id string) (Node, bool) {
	for _, node := range g.nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func (g *Graph) reachesNodeLocked(start, goal string) bool {
	if start == "" || goal == "" {
		return false
	}
	_, ok := g.reachableNodesLocked(start)[goal]
	return ok
}

func (g *Graph) reachableNodesLocked(start string) map[string]struct{} {
	adj := make(map[string][]string, len(g.tasks)*3)
	for _, edge := range g.edges {
		adj[edge.From] = append(adj[edge.From], edge.To)
	}
	seen := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return seen
}

// Output returns an immutable paired checkpoint by node ID.
func (g *Graph) Output(nodeID string) (Output, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	output, ok := g.outputs[nodeID]
	return output, ok
}

func (g *Graph) commitOutput(output Output) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	node, ok := g.nodeByIDLocked(output.Node.ID)
	if !ok || node != output.Node {
		return fmt.Errorf("%w: %q", ErrUnknownNode, output.Node.ID)
	}
	if output.FilesRef == "" || output.MemoryRef == "" {
		return fmt.Errorf("coordination: output %q requires paired file and memory references", node.ID)
	}
	if existing, ok := g.outputs[node.ID]; ok {
		if existing != output {
			return fmt.Errorf("coordination: output %q is immutable", node.ID)
		}
		return nil
	}
	before := g.stateLocked()
	g.outputs[node.ID] = output
	g.revision++
	return g.saveOrRestoreLocked(before)
}

// Connect adds an ordinary dependency. Replaying an existing edge is harmless.
func (g *Graph) Connect(from, to string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, edge := range g.edges {
		if edge.From == from && edge.To == to {
			return nil
		}
	}
	frozen, err := g.inputFrozenLocked(to)
	if err != nil {
		return err
	}
	if frozen {
		return fmt.Errorf("%w: node %q", ErrGraphBusy, to)
	}
	if err := g.validateSourceLocked(from); err != nil {
		return err
	}
	before := g.stateLocked()
	if err := g.connectLocked(Edge{From: from, To: to}); err != nil {
		return err
	}
	g.revision++
	return g.saveOrRestoreLocked(before)
}

func (g *Graph) connectLocked(edge Edge) error {
	for _, id := range []string{edge.From, edge.To} {
		if _, ok := g.nodeByIDLocked(id); !ok {
			return fmt.Errorf("%w: %q", ErrUnknownNode, id)
		}
	}
	if g.reachesNodeLocked(edge.To, edge.From) {
		return fmt.Errorf("%w: %q → %q", ErrCycle, edge.From, edge.To)
	}
	g.edges = append(g.edges, edge)
	return nil
}

// Continue starts a new activation of an idle persistent task from its last output.
func (g *Graph) Continue(taskID, info string) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if !task.Persistent || task.Outcome != OutcomeIdle {
		return Task{}, fmt.Errorf("coordination: task %q must be persistent and idle to continue", taskID)
	}
	if g.runners[taskID] != nil {
		return Task{}, ErrGraphBusy
	}
	info = strings.TrimSpace(info)
	if info == "" {
		return Task{}, fmt.Errorf("coordination: continuation info is required")
	}
	previous := task.Verifier.ID
	if _, ok := g.outputs[previous]; !ok {
		return Task{}, fmt.Errorf("coordination: task %q has no completed verifier output", taskID)
	}
	before := g.stateLocked()
	task.Info = info
	task.Outcome = OutcomeActive
	g.addActivationLocked(&task)
	for i := range g.tasks {
		if g.tasks[i].ID == taskID {
			g.tasks[i] = task
			break
		}
	}
	g.edges = append(g.edges, Edge{From: previous, To: task.Planner.ID})
	g.revision++
	if err := g.saveAndProjectLocked(before); err != nil {
		return Task{}, err
	}
	return task, nil
}

// CloseTask closes one persistent task and cancels only its current activation.
func (g *Graph) CloseTask(taskID string) error {
	g.mu.Lock()
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if !task.Persistent {
		g.mu.Unlock()
		return fmt.Errorf("coordination: task %q is not persistent", taskID)
	}
	if task.Outcome == OutcomeClosed {
		g.mu.Unlock()
		return nil
	}
	before := g.stateLocked()
	for i := range g.tasks {
		if g.tasks[i].ID == taskID {
			g.tasks[i].Outcome = OutcomeClosed
			break
		}
	}
	g.revision++
	if err := g.saveOrRestoreLocked(before); err != nil {
		g.mu.Unlock()
		return err
	}
	running := g.runners[taskID]
	g.mu.Unlock()
	if running != nil {
		running.cancel()
	}
	return nil
}

func (g *Graph) addCheckpointNode(node Node) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.nodeByIDLocked(node.ID); ok {
		if existing != node {
			return fmt.Errorf("coordination: node %q is immutable", node.ID)
		}
		return nil
	}
	task, ok := g.taskByIDLocked(node.TaskID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownTask, node.TaskID)
	}
	if !currentActivationNode(task, node) || !validNodeForTask(task, node) {
		return fmt.Errorf("coordination: invalid checkpoint node %q", node.ID)
	}
	before := g.stateLocked()
	g.nodes = append(g.nodes, node)
	g.revision++
	return g.saveOrRestoreLocked(before)
}

func currentActivationNode(task Task, node Node) bool {
	return strings.HasPrefix(node.ID, fmt.Sprintf("%s:%d:", task.ID, task.Activation))
}

func validRole(role string) bool {
	return role == RolePlanner || role == RoleExecutor || role == RoleVerifier
}

func validNodeForTask(task Task, node Node) bool {
	if node.TaskID != task.ID || !validRole(node.Role) {
		return false
	}
	suffix, ok := strings.CutPrefix(node.ID, task.ID+":")
	if !ok {
		return false
	}
	raw, remainder, ok := strings.Cut(suffix, ":")
	if !ok {
		return false
	}
	activation, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || activation == 0 || activation > task.Activation {
		return false
	}
	role, checkpoint, extra := strings.Cut(remainder, ":")
	return role == node.Role && (!extra || checkpoint != "")
}

func (g *Graph) validateSourceLocked(nodeID string) error {
	node, ok := g.nodeByIDLocked(nodeID)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownNode, nodeID)
	}
	if _, ok := g.outputs[nodeID]; ok {
		return nil
	}
	task, ok := g.taskByIDLocked(node.TaskID)
	if !ok || !currentActivationNode(task, node) || task.Outcome == OutcomeClosed {
		return fmt.Errorf("coordination: source %q has no committed output and cannot run", nodeID)
	}
	return nil
}
