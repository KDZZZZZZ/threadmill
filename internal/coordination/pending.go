package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidPending 表示提交的期望子图不合法。
var ErrInvalidPending = errors.New("coordination: invalid pending subgraph")

// PendingSpawn 是一条期望的 spawn/join，info 是子任务目标。
type PendingSpawn struct {
	From string `json:"from"`
	Join string `json:"join"`
	Info string `json:"info"`
}

// PendingRoot 是一个期望的根任务。
type PendingRoot struct {
	Info string `json:"info"`
}

// PendingSubgraph 是尚未执行切片的完整期望状态。
// 根按序号对齐：少于现有根数会失败，多出的新建；spawn 仍按 from/join 匹配。
type PendingSubgraph struct {
	Roots  []PendingRoot  `json:"roots,omitempty"`
	Spawns []PendingSpawn `json:"spawns"`
}

// ReplacePending 用期望态替换尚未执行的切片。失败（含成环）时图不变。
func (g *Graph) ReplacePending(ctx context.Context, next PendingSubgraph) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if g == nil {
		return Snapshot{}, fmt.Errorf("replace pending: nil graph")
	}
	g.mu.Lock()
	if g.executing {
		g.mu.Unlock()
		return Snapshot{}, ErrGraphBusy
	}
	nextGraph := &Graph{
		tasks:     append([]Task(nil), g.tasks...),
		edges:     append([]Edge(nil), g.edges...),
		nextID:    g.nextID,
		helps:     cloneHelpStates(g.helps),
		statePath: g.statePath,
	}
	if err := nextGraph.applyPendingLocked(next); err != nil {
		g.mu.Unlock()
		return Snapshot{}, err
	}
	if err := completedTasksUnchanged(g, nextGraph); err != nil {
		g.mu.Unlock()
		return Snapshot{}, err
	}
	nextGraph.revision = g.revision + 1
	if err := nextGraph.saveLocked(); err != nil {
		g.mu.Unlock()
		return Snapshot{}, err
	}
	snap := nextGraph.snapshotLocked()
	if err := emitTasks(g.taskSink, snap.Tasks); err != nil {
		rollbackErr := g.saveLocked()
		g.mu.Unlock()
		return Snapshot{}, errors.Join(err, rollbackErr)
	}
	g.tasks = nextGraph.tasks
	g.edges = nextGraph.edges
	g.nextID = nextGraph.nextID
	g.helps = nextGraph.helps
	g.revision = nextGraph.revision
	g.mu.Unlock()
	return snap, nil
}

func completedTasksUnchanged(current, next *Graph) error {
	for _, task := range current.tasks {
		if task.Outcome == OutcomeActive {
			continue
		}
		candidate, ok := next.taskByIDLocked(task.ID)
		if !ok || !sameTask(task, candidate) || !sameEdges(
			incidentEdges(current, task), incidentEdges(next, task),
		) {
			return fmt.Errorf("%w: completed task %q is immutable", ErrInvalidPending, task.ID)
		}
	}
	return nil
}

func sameTask(left, right Task) bool {
	return left.ID == right.ID &&
		left.Info == right.Info &&
		left.Env == right.Env &&
		left.Planner == right.Planner &&
		left.Executor == right.Executor &&
		left.Verifier == right.Verifier &&
		left.Outcome == right.Outcome &&
		left.RunPolicy == right.RunPolicy
}

func incidentEdges(graph *Graph, task Task) []Edge {
	nodes := map[string]struct{}{
		task.Planner.ID:  {},
		task.Executor.ID: {},
		task.Verifier.ID: {},
	}
	var edges []Edge
	for _, edge := range graph.edges {
		_, from := nodes[edge.From]
		_, to := nodes[edge.To]
		if from || to {
			edges = append(edges, edge)
		}
	}
	return edges
}

func sameEdges(left, right []Edge) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (g *Graph) applyPendingLocked(next PendingSubgraph) error {
	roots := g.rootTasksLocked()
	if len(next.Roots) < 1 {
		return fmt.Errorf("%w: roots required", ErrInvalidPending)
	}
	if len(next.Roots) < len(roots) {
		return fmt.Errorf("%w: cannot remove root tasks", ErrUnspawnRoot)
	}
	for len(roots) < len(next.Roots) {
		g.addTaskLocked("")
		roots = g.rootTasksLocked()
	}
	for i, want := range next.Roots {
		g.setInfoLocked(roots[i].ID, want.Info)
	}

	type spawnKey struct {
		From string
		Join string
		Info string
	}
	desired := make(map[spawnKey]PendingSpawn, len(next.Spawns))
	for _, spawn := range next.Spawns {
		spawn.From = strings.TrimSpace(spawn.From)
		spawn.Join = strings.TrimSpace(spawn.Join)
		if spawn.From == "" || spawn.Join == "" {
			return fmt.Errorf("%w: spawn from and join are required", ErrInvalidPending)
		}
		desired[spawnKey{From: spawn.From, Join: spawn.Join, Info: spawn.Info}] = spawn
	}

	have := make(map[spawnKey]string)
	for _, task := range g.tasks {
		pair, ok := g.spawnPairLocked(task)
		if !ok {
			continue
		}
		have[spawnKey{From: pair.From, Join: pair.Join, Info: task.Info}] = task.ID
	}
	for key, spawn := range desired {
		if id, ok := have[key]; ok {
			g.setInfoLocked(id, spawn.Info)
			continue
		}
		child, err := g.spawnLocked(spawn.From, spawn.Join)
		if err != nil {
			return err
		}
		g.setInfoLocked(child.ID, spawn.Info)
	}

	for {
		extra := false
		for _, task := range append([]Task(nil), g.tasks...) {
			pair, ok := g.spawnPairLocked(task)
			if !ok {
				continue
			}
			if _, keep := desired[spawnKey{From: pair.From, Join: pair.Join, Info: task.Info}]; keep {
				continue
			}
			if _, err := g.unspawnLocked(task.ID); err != nil {
				return err
			}
			extra = true
			break
		}
		if !extra {
			break
		}
	}
	return nil
}

func (g *Graph) rootTasksLocked() []Task {
	out := make([]Task, 0)
	for _, task := range g.tasks {
		if g.spawnedFromLocked(task) == "" {
			out = append(out, task)
		}
	}
	return out
}

func (g *Graph) setInfoLocked(id, info string) {
	for i := range g.tasks {
		if g.tasks[i].ID == id {
			g.tasks[i].Info = info
			return
		}
	}
}

func (g *Graph) spawnPairLocked(task Task) (PendingSpawn, bool) {
	var pair PendingSpawn
	found := false
	for _, edge := range g.edges {
		if edge.Kind == EdgeKindSpawn && edge.To == task.Planner.ID {
			pair.From = edge.From
			found = true
		}
		if edge.Kind == EdgeKindJoin && edge.From == task.Verifier.ID {
			pair.Join = edge.To
		}
	}
	return pair, found
}
