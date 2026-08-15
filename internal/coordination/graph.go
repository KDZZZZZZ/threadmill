// Package coordination 定义 Threadmill 的协调图。
//
// 一个 task 按顺序有且仅有 planner、executor、verifier。
// 任意角色都可作为新 task 的起始点（spawn）和合入点（join）。
// 图是进程内全局单例，由 Default 返回。
package coordination

import (
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
	EdgeKindSequence = "sequence"
	EdgeKindSpawn    = "spawn"
	EdgeKindJoin     = "join"
)

// ErrUnknownNode 表示 spawn 的起始点或合入点不在图中。
var ErrUnknownNode = errors.New("coordination: unknown node")

var defaultGraph = newGraph()

// Default 返回全局协调图单例。
func Default() *Graph {
	return defaultGraph
}

// Node 是某个 task 里的一个角色。
type Node struct {
	ID     string
	TaskID string
	Role   string
}

// Task 是 planner → executor → verifier 的固定三角色序列。
type Task struct {
	ID       string
	Planner  Node
	Executor Node
	Verifier Node
}

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
	mu     sync.Mutex
	tasks  []Task
	edges  []Edge
	nextID uint64
}

func newGraph() *Graph {
	return &Graph{
		tasks: []Task{},
		edges: []Edge{},
	}
}

func (g *Graph) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tasks = []Task{}
	g.edges = []Edge{}
	g.nextID = 0
}

// AddTask 追加一个独立 task，内部连好 planner → executor → verifier。
func (g *Graph) AddTask() Task {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addTaskLocked()
}

// Spawn 从已有角色节点拉出新 task，并在合入点接回。
// 新 task 仍是 planner → executor → verifier；from 连到其 planner，其 verifier 连到 join。
func (g *Graph) Spawn(from, join string) (Task, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodeByIDLocked(from); !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownNode, from)
	}
	if _, ok := g.nodeByIDLocked(join); !ok {
		return Task{}, fmt.Errorf("%w: %q", ErrUnknownNode, join)
	}
	child := g.addTaskLocked()
	g.edges = append(g.edges,
		Edge{From: from, To: child.Planner.ID, Kind: EdgeKindSpawn},
		Edge{From: child.Verifier.ID, To: join, Kind: EdgeKindJoin},
	)
	return child, nil
}

// Task 按 ID 查找 task；不存在时 ok 为 false。
func (g *Graph) Task(id string) (Task, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.taskByIDLocked(id)
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

func (g *Graph) addTaskLocked() Task {
	g.nextID++
	id := fmt.Sprintf("task-%d", g.nextID)
	task := Task{
		ID: id,
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
