package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidPending 表示提交的期望子图不合法。
var ErrInvalidPending = errors.New("coordination: invalid pending subgraph")

// PendingSpawn 是一条期望的 spawn/join。
type PendingSpawn struct {
	From string `json:"from"`
	Join string `json:"join"`
}

// PendingSubgraph 是尚未执行切片的完整期望状态。
// ponytail: 当前图没有 PhaseEndpoint；用根 task 数量 + spawn/join 对表达期望拓扑。
type PendingSubgraph struct {
	Roots  int            `json:"roots,omitempty"`
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
	defer g.mu.Unlock()
	if g.executing {
		return Snapshot{}, ErrGraphBusy
	}
	nextGraph := &Graph{
		tasks:  append([]Task(nil), g.tasks...),
		edges:  append([]Edge(nil), g.edges...),
		nextID: g.nextID,
	}
	if err := nextGraph.applyPendingLocked(next); err != nil {
		return Snapshot{}, err
	}
	g.tasks = nextGraph.tasks
	g.edges = nextGraph.edges
	g.nextID = nextGraph.nextID
	g.revision++
	return g.snapshotLocked(), nil
}

func (g *Graph) applyPendingLocked(next PendingSubgraph) error {
	roots := g.rootCountLocked()
	wantRoots := next.Roots
	if wantRoots == 0 {
		wantRoots = roots
	}
	if wantRoots < 1 {
		return fmt.Errorf("%w: roots required", ErrInvalidPending)
	}
	if wantRoots < roots {
		return fmt.Errorf("%w: cannot remove root tasks", ErrUnspawnRoot)
	}
	for roots < wantRoots {
		g.addTaskLocked("")
		roots++
	}

	desired := make(map[PendingSpawn]struct{}, len(next.Spawns))
	for _, spawn := range next.Spawns {
		spawn.From = strings.TrimSpace(spawn.From)
		spawn.Join = strings.TrimSpace(spawn.Join)
		if spawn.From == "" || spawn.Join == "" {
			return fmt.Errorf("%w: spawn from and join are required", ErrInvalidPending)
		}
		desired[spawn] = struct{}{}
	}

	have := make(map[PendingSpawn]struct{})
	for _, task := range g.tasks {
		pair, ok := g.spawnPairLocked(task)
		if ok {
			have[pair] = struct{}{}
		}
	}
	for spawn := range desired {
		if _, ok := have[spawn]; ok {
			continue
		}
		if _, err := g.spawnLocked(spawn.From, spawn.Join); err != nil {
			return err
		}
	}

	for {
		extra := false
		for _, task := range append([]Task(nil), g.tasks...) {
			pair, ok := g.spawnPairLocked(task)
			if !ok {
				continue
			}
			if _, keep := desired[pair]; keep {
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

func (g *Graph) rootCountLocked() int {
	n := 0
	for _, task := range g.tasks {
		if g.spawnedFromLocked(task) == "" {
			n++
		}
	}
	return n
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
