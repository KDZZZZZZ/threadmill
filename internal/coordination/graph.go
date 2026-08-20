// Package coordination 定义 Threadmill 的协调图。
//
// 一个 task 按顺序有且仅有 planner、executor、verifier。
// 任意角色都可作为新 task 的起始点（spawn）和合入点（join）。
// 图是进程内全局单例，由 Default 返回。
// 调度也由图负责：先 fork 环境、再组装 agent，然后对该角色执行 ReAct（Ask）。
package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const (
	RolePlanner  = "planner"
	RoleExecutor = "executor"
	RoleVerifier = "verifier"
)

const (
	OutcomeActive   = "active"
	OutcomeDone     = "done"
	OutcomeCanceled = "canceled"
	OutcomeFailed   = "failed"

	RunPolicyEnabled = "enabled"
	RunPolicyHeld    = "held"
)

const (
	EdgeKindSequence = "sequence"
	EdgeKindSpawn    = "spawn"
	EdgeKindJoin     = "join"
)

// ErrUnknownNode 表示 spawn 的起始点或合入点不在图中。
var ErrUnknownNode = errors.New("coordination: unknown node")

// ErrJoinCycle 表示 spawn/join 会在 Ask 前形成开始依赖环，或 join 跨了任务树。
var ErrJoinCycle = errors.New("coordination: join cycle")

// ErrGraphBusy 表示并发 Run，或改图触及已经开始执行的切片。
var ErrGraphBusy = errors.New("coordination: graph is executing")

// ErrUnspawnRoot 表示不能拆掉独立根 task。
var ErrUnspawnRoot = errors.New("coordination: cannot unspawn root task")

// ErrUnknownRevision 表示请求的图 revision 不是当前 revision。
var ErrUnknownRevision = errors.New("coordination: unknown revision")

var defaultGraph = New()

// Default 返回全局协调图单例。
func Default() *Graph {
	return defaultGraph
}

// New 返回一张空的协调图。
func New() *Graph {
	return &Graph{
		tasks: []Task{},
		edges: []Edge{},
	}
}

// Node 是某个 task 里的一个角色。
type Node struct {
	ID     string
	TaskID string
	Role   string
}

// Env 是 task 的版本句柄。Spawn 从父环境 fork；Join 只声明合入范围，不合内容。
type Env struct {
	ID       string
	ParentID string // fork 来源；根为空
}

// Task 是 planner → executor → verifier 的固定三角色序列。
// SpawnedFrom / Joins / JoinedBy 由图上的 spawn、join 边解析，不单独存一份。
type Task struct {
	ID          string
	Info        string // 任务目标与验收标准
	Env         Env
	Planner     Node
	Executor    Node
	Verifier    Node
	Outcome     string   // active | done | canceled | failed
	RunPolicy   string   // enabled | held
	SpawnedFrom string   // 拉出本 task 的父 task；根为空
	Joins       []string // 本 task 合入的 task
	JoinedBy    []string // 合入本 task 的 task
}

// TaskSink 原子接收协调图中全部 task info 的自动投影。
type TaskSink func([]Task) error

// Sequence 按固定顺序返回三个角色节点。
func (t Task) Sequence() []Node {
	return []Node{t.Planner, t.Executor, t.Verifier}
}

// Edge 是角色节点之间的有向关系。
type Edge struct {
	From string
	To   string
	Kind string
}

// Graph 是可并发访问的协调图。
type Graph struct {
	mu        sync.Mutex
	tasks     []Task
	edges     []Edge
	nextID    uint64
	helps     []helpState
	progress  ProgressStore
	help      *helpCoordinator
	taskSink  TaskSink
	statePath string
	executing bool
	running   *runner
	revision  int64
}

func newGraph() *Graph {
	return New()
}

// SetProgressStore 设置进行中 task 的进度存储。入口 Run 成功结束后扔掉整棵子树的进度。
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

func (g *Graph) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tasks = []Task{}
	g.edges = []Edge{}
	g.nextID = 0
	g.helps = nil
	g.executing = false
	g.running = nil
	g.revision = 0
}

// AddTask 追加一个图上独立的 task，内部连好 planner → executor → verifier。
// root task 的环境按顺序从前一个 root fork，便于后续 task 增量续作。
func (g *Graph) AddTask() Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	task := g.decorateLocked(g.addRootLocked())
	g.revision++
	return task
}

// Spawn 从已有角色节点拉出新 task，并在合入点接回。
// 新 task 仍是 planner → executor → verifier；from 连到其 planner，其 verifier 连到 join。
// join 与 from 必须同属一棵任务树，且 start/ask 依赖不能成环（含 Spawn(x, x)），否则返回 ErrJoinCycle。
func (g *Graph) Spawn(from, join string) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.executing {
		return Task{}, ErrGraphBusy
	}
	before := g.stateLocked()
	child, err := g.spawnLocked(from, join)
	if err != nil {
		return Task{}, err
	}
	g.revision++
	if err := g.saveOrRestoreLocked(before); err != nil {
		return Task{}, err
	}
	return child, nil
}

func (g *Graph) spawnLocked(from, join string) (Task, error) {
	fromNode, ok := g.nodeByIDLocked(from)
	if !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownNode, from)
	}
	joinNode, ok := g.nodeByIDLocked(join)
	if !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownNode, join)
	}
	if g.treeRootLocked(fromNode.TaskID) != g.treeRootLocked(joinNode.TaskID) ||
		g.reachesStartAskLocked(join+"/start", from+"/ask") {
		return Task{}, fmt.Errorf("%w: %q -> %q", ErrJoinCycle, from, join)
	}
	parent, ok := g.taskByIDLocked(fromNode.TaskID)
	if !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownNode, from)
	}
	child := g.addTaskLocked(parent.Env.ID)
	g.edges = append(g.edges,
		Edge{From: from, To: child.Planner.ID, Kind: EdgeKindSpawn},
		Edge{From: child.Verifier.ID, To: join, Kind: EdgeKindJoin},
	)
	return g.decorateLocked(child), nil
}

// Snapshot 是图的只读拷贝，供 manager 查看。
type Snapshot struct {
	Revision  int64  `json:"revision"`
	Executing bool   `json:"executing"`
	Tasks     []Task `json:"tasks"`
	Edges     []Edge `json:"edges"`
}

// Snapshot 返回当前 tasks 和边；Run 期间 executing 为 true。
func (g *Graph) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked()
}

func (g *Graph) snapshotLocked() Snapshot {
	tasks := make([]Task, 0, len(g.tasks))
	for _, task := range g.tasks {
		tasks = append(tasks, g.decorateLocked(task))
	}
	return Snapshot{
		Revision:  g.revision,
		Executing: g.executing,
		Tasks:     tasks,
		Edges:     append([]Edge(nil), g.edges...),
	}
}

// SnapshotAt 返回 revision-consistent 快照；revision=0 表示最新。
func (g *Graph) SnapshotAt(ctx context.Context, revision int64) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if g == nil {
		return Snapshot{}, fmt.Errorf("snapshot: nil graph")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	snap := g.snapshotLocked()
	if revision != 0 && revision != snap.Revision {
		return Snapshot{}, fmt.Errorf("%w: %d", ErrUnknownRevision, revision)
	}
	return snap, nil
}

// Unspawn 拆掉尚未开跑的子 task 及其子孙。根 task 返回 ErrUnspawnRoot；Run 期间返回 ErrGraphBusy。
func (g *Graph) Unspawn(taskID string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.executing {
		return nil, ErrGraphBusy
	}
	before := g.stateLocked()
	removed, err := g.unspawnLocked(taskID)
	if err != nil {
		return nil, err
	}
	g.revision++
	if err := g.saveOrRestoreLocked(before); err != nil {
		return nil, err
	}
	return removed, nil
}

func (g *Graph) unspawnLocked(taskID string) ([]string, error) {
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if g.spawnedFromLocked(task) == "" {
		return nil, fmt.Errorf("%w: %q", ErrUnspawnRoot, taskID)
	}
	remove := g.spawnedSubtreeLocked(taskID)
	nodeIDs := make(map[string]struct{})
	removed := make([]string, 0, len(remove))
	for _, existing := range g.tasks {
		if _, ok := remove[existing.ID]; !ok {
			continue
		}
		removed = append(removed, existing.ID)
		for _, node := range existing.Sequence() {
			nodeIDs[node.ID] = struct{}{}
		}
	}
	edges := g.edges[:0]
	for _, edge := range g.edges {
		if _, ok := nodeIDs[edge.From]; ok {
			continue
		}
		if _, ok := nodeIDs[edge.To]; ok {
			continue
		}
		edges = append(edges, edge)
	}
	g.edges = edges
	tasks := g.tasks[:0]
	for _, existing := range g.tasks {
		if _, ok := remove[existing.ID]; ok {
			continue
		}
		tasks = append(tasks, existing)
	}
	g.tasks = tasks
	return removed, nil
}

func (g *Graph) spawnedSubtreeLocked(rootID string) map[string]struct{} {
	seen := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		task, ok := g.taskByIDLocked(id)
		if !ok {
			continue
		}
		for _, node := range task.Sequence() {
			for _, edge := range g.edges {
				if edge.Kind != EdgeKindSpawn || edge.From != node.ID {
					continue
				}
				to, ok := g.nodeByIDLocked(edge.To)
				if !ok || to.TaskID == "" {
					continue
				}
				if _, dup := seen[to.TaskID]; dup {
					continue
				}
				seen[to.TaskID] = struct{}{}
				queue = append(queue, to.TaskID)
			}
		}
	}
	return seen
}

// Task 按 ID 查找 task；不存在时 ok 为 false。
func (g *Graph) Task(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	task, ok := g.taskByIDLocked(id)
	if !ok {
		return Task{}, false
	}
	return g.decorateLocked(task), true
}

// Downstream 返回该节点指出的下游角色节点。
// 按边的原有顺序且按 ID 去重；节点不存在时返回空切片。
func (g *Graph) Downstream(nodeID string) []Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	nodes := make([]Node, 0)
	if _, ok := g.nodeByIDLocked(nodeID); !ok {
		return nodes
	}

	seen := make(map[string]struct{})
	for _, edge := range g.edges {
		if edge.From != nodeID || edge.To == "" {
			continue
		}
		if _, dup := seen[edge.To]; dup {
			continue
		}
		node, ok := g.nodeByIDLocked(edge.To)
		if !ok {
			continue
		}
		seen[edge.To] = struct{}{}
		nodes = append(nodes, node)
	}
	return nodes
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

// IncomingJoins 返回以 join 边指向该节点的上游角色节点。
// 按边的原有顺序且按 ID 去重；节点不存在时返回空切片。
func (g *Graph) IncomingJoins(nodeID string) []Node {
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
		if edge.Kind != EdgeKindJoin || edge.To != nodeID || edge.From == "" {
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

// SpawnedTasks 返回从该角色节点拉出的子 task，按 spawn 边顺序。
func (g *Graph) SpawnedTasks(nodeID string) []Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	tasks := make([]Task, 0)
	if nodeID == "" {
		return tasks
	}
	seen := make(map[string]struct{})
	for _, edge := range g.edges {
		if edge.Kind != EdgeKindSpawn || edge.From != nodeID {
			continue
		}
		to, ok := g.nodeByIDLocked(edge.To)
		if !ok || to.TaskID == "" {
			continue
		}
		if _, dup := seen[to.TaskID]; dup {
			continue
		}
		task, ok := g.taskByIDLocked(to.TaskID)
		if !ok {
			continue
		}
		seen[to.TaskID] = struct{}{}
		tasks = append(tasks, g.decorateLocked(task))
	}
	return tasks
}

// Forks 返回从该环境 fork 出去的子环境，按 task 创建顺序。
func (g *Graph) Forks(envID string) []Env {
	g.mu.Lock()
	defer g.mu.Unlock()
	envs := make([]Env, 0)
	if envID == "" {
		return envs
	}
	for _, task := range g.tasks {
		if task.Env.ParentID == envID {
			envs = append(envs, task.Env)
		}
	}
	return envs
}

// Impact 返回将合入该 task 环境的子环境，按 join 边顺序。
func (g *Graph) Impact(taskID string) []Env {
	g.mu.Lock()
	defer g.mu.Unlock()
	envs := make([]Env, 0)
	for _, id := range g.joinedByLocked(taskID) {
		task, ok := g.taskByIDLocked(id)
		if !ok || task.Env.ID == "" {
			continue
		}
		envs = append(envs, task.Env)
	}
	return envs
}

func (g *Graph) addTaskLocked(parentEnvID string) Task {
	g.nextID++
	id := fmt.Sprintf("task-%d", g.nextID)
	task := Task{
		ID:        id,
		Outcome:   OutcomeActive,
		RunPolicy: RunPolicyEnabled,
		Env: Env{
			ID:       fmt.Sprintf("env-%d", g.nextID),
			ParentID: parentEnvID,
		},
		Planner: Node{
			ID:     id + ":" + RolePlanner,
			TaskID: id,
			Role:   RolePlanner,
		},
		Executor: Node{
			ID:     id + ":" + RoleExecutor,
			TaskID: id,
			Role:   RoleExecutor,
		},
		Verifier: Node{
			ID:     id + ":" + RoleVerifier,
			TaskID: id,
			Role:   RoleVerifier,
		},
	}
	g.tasks = append(g.tasks, task)
	g.edges = append(g.edges,
		Edge{From: task.Planner.ID, To: task.Executor.ID, Kind: EdgeKindSequence},
		Edge{From: task.Executor.ID, To: task.Verifier.ID, Kind: EdgeKindSequence},
	)
	return task
}

func (g *Graph) addRootLocked() Task {
	parentEnvID := ""
	roots := g.rootTasksLocked()
	if len(roots) > 0 {
		parentEnvID = roots[len(roots)-1].Env.ID
	}
	return g.addTaskLocked(parentEnvID)
}

func (g *Graph) taskByIDLocked(id string) (Task, bool) {
	if id == "" {
		return Task{}, false
	}
	// ponytail: 线性扫描，图变大后再建索引
	for _, task := range g.tasks {
		if task.ID == id {
			return task, true
		}
	}
	return Task{}, false
}

func (g *Graph) nodeByIDLocked(id string) (Node, bool) {
	if id == "" {
		return Node{}, false
	}
	for _, task := range g.tasks {
		for _, node := range task.Sequence() {
			if node.ID == id {
				return node, true
			}
		}
	}
	return Node{}, false
}

func (g *Graph) treeRootLocked(taskID string) string {
	id := taskID
	seen := make(map[string]struct{})
	for id != "" {
		if _, dup := seen[id]; dup {
			return id
		}
		seen[id] = struct{}{}
		task, ok := g.taskByIDLocked(id)
		if !ok {
			return id
		}
		parent := g.spawnedFromLocked(task)
		if parent == "" {
			return id
		}
		id = parent
	}
	return id
}

func (g *Graph) reachesStartAskLocked(start, goal string) bool {
	if start == "" || goal == "" {
		return false
	}
	adj := make(map[string][]string)
	add := func(from, to string) {
		adj[from] = append(adj[from], to)
	}
	for _, task := range g.tasks {
		for _, node := range task.Sequence() {
			add(node.ID+"/start", node.ID+"/ask")
		}
	}
	for _, edge := range g.edges {
		add(edge.From+"/ask", edge.To+"/start")
	}
	if start == goal {
		return true
	}
	seen := map[string]struct{}{start: {}}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if next == goal {
				return true
			}
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	return false
}

func (g *Graph) decorateLocked(task Task) Task {
	task.SpawnedFrom = g.spawnedFromLocked(task)
	task.Joins = g.joinsLocked(task)
	task.JoinedBy = g.joinedByLocked(task.ID)
	return task
}

func (g *Graph) spawnedFromLocked(task Task) string {
	for _, edge := range g.edges {
		if edge.Kind != EdgeKindSpawn || edge.To != task.Planner.ID {
			continue
		}
		from, ok := g.nodeByIDLocked(edge.From)
		if ok {
			return from.TaskID
		}
	}
	return ""
}

func (g *Graph) joinsLocked(task Task) []string {
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, edge := range g.edges {
		if edge.Kind != EdgeKindJoin || edge.From != task.Verifier.ID {
			continue
		}
		to, ok := g.nodeByIDLocked(edge.To)
		if !ok || to.TaskID == "" {
			continue
		}
		if _, dup := seen[to.TaskID]; dup {
			continue
		}
		seen[to.TaskID] = struct{}{}
		ids = append(ids, to.TaskID)
	}
	return ids
}

func (g *Graph) joinedByLocked(taskID string) []string {
	ids := make([]string, 0)
	if taskID == "" {
		return ids
	}

	seen := make(map[string]struct{})
	for _, edge := range g.edges {
		if edge.Kind != EdgeKindJoin {
			continue
		}
		to, ok := g.nodeByIDLocked(edge.To)
		if !ok || to.TaskID != taskID {
			continue
		}
		from, ok := g.nodeByIDLocked(edge.From)
		if !ok || from.TaskID == "" {
			continue
		}
		if _, dup := seen[from.TaskID]; dup {
			continue
		}
		seen[from.TaskID] = struct{}{}
		ids = append(ids, from.TaskID)
	}
	return ids
}

func (g *Graph) taskTree(rootID string) []string {
	if rootID == "" {
		return nil
	}
	seen := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		task, ok := g.Task(id)
		if !ok {
			continue
		}
		for _, node := range task.Sequence() {
			for _, child := range g.SpawnedTasks(node.ID) {
				if _, dup := seen[child.ID]; dup {
					continue
				}
				seen[child.ID] = struct{}{}
				queue = append(queue, child.ID)
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
}
