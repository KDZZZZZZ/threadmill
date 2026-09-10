package coordination

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalidPending indicates an invalid desired graph.
var ErrInvalidPending = errors.New("coordination: invalid pending subgraph")

// PendingTask describes an existing task or a new independent task.
// Omitted IDs allocate task-N; existing IDs preserve omitted info and run policy.
// Persistent tasks keep that property until closed through CloseTask.
type PendingTask struct {
	ID         string `json:"id,omitempty"`
	Info       string `json:"info,omitempty"`
	RunPolicy  string `json:"run_policy,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`
}

// PendingSubgraph is the complete desired pending task and dependency set.
type PendingSubgraph struct {
	Tasks []PendingTask `json:"tasks"`
	Edges []Edge        `json:"edges"`
}

// ReplacePending atomically replaces the editable graph and projects task info.
func (g *Graph) ReplacePending(ctx context.Context, next PendingSubgraph) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if g == nil {
		return Snapshot{}, fmt.Errorf("replace pending: nil graph")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	if err := g.applyPendingLocked(next); err != nil {
		return Snapshot{}, err
	}
	g.revision++
	if err := g.saveAndProjectLocked(before); err != nil {
		return Snapshot{}, err
	}
	snap := g.snapshotLocked()
	return snap, nil
}

// applyPendingLocked validates a complete candidate before exposing any mutation.
func (g *Graph) applyPendingLocked(next PendingSubgraph) error {
	candidate := New()
	candidate.applyStateLocked(g.stateLocked())
	if err := candidate.replacePendingLocked(next); err != nil {
		return err
	}
	if err := immutableHistoryUnchanged(g, candidate); err != nil {
		return err
	}
	for _, running := range g.runners {
		if err := runningSliceUnchanged(g, candidate, running); err != nil {
			return err
		}
	}
	if err := persistedInputsUnchanged(g, candidate); err != nil {
		return err
	}
	state := candidate.stateLocked()
	if err := validateGraphState(state); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPending, err)
	}
	g.applyStateLocked(state)
	return nil
}

// addPendingLocked applies an additive change for an existing Help request.
// The caller owns the graph lock, revision and durable transaction.
func (g *Graph) addPendingLocked(next PendingSubgraph) error {
	full := PendingSubgraph{Edges: append([]Edge(nil), g.edges...)}
	for _, task := range g.tasks {
		full.Tasks = append(full.Tasks, PendingTask{ID: task.ID, Info: task.Info, RunPolicy: task.RunPolicy, Persistent: task.Persistent})
	}
	full.Tasks = append(full.Tasks, next.Tasks...)
	seen := make(map[Edge]bool, len(full.Edges))
	for _, edge := range full.Edges {
		seen[edge] = true
	}
	for _, edge := range next.Edges {
		if !seen[edge] {
			full.Edges = append(full.Edges, edge)
			seen[edge] = true
		}
	}
	return g.applyPendingLocked(full)
}

func (g *Graph) replacePendingLocked(next PendingSubgraph) error {
	oldEdges := append([]Edge(nil), g.edges...)
	oldSet := make(map[Edge]bool, len(oldEdges))
	for _, edge := range oldEdges {
		oldSet[edge] = true
	}
	wanted := make(map[string]PendingTask, len(next.Tasks))
	for _, want := range next.Tasks {
		want.Info = strings.TrimSpace(want.Info)
		want.RunPolicy = strings.TrimSpace(want.RunPolicy)
		if want.RunPolicy != "" && want.RunPolicy != RunPolicyEnabled && want.RunPolicy != RunPolicyHeld {
			return fmt.Errorf("%w: invalid run policy %q", ErrInvalidPending, want.RunPolicy)
		}
		if want.ID != "" && !validTaskID(want.ID) {
			return fmt.Errorf("%w: invalid task ID %q", ErrInvalidPending, want.ID)
		}
		if _, duplicate := wanted[want.ID]; duplicate && want.ID != "" {
			return fmt.Errorf("%w: duplicate task %q", ErrInvalidPending, want.ID)
		}
		if _, exists := g.taskByIDLocked(want.ID); !exists {
			if want.Info == "" {
				return fmt.Errorf("%w: task info is required", ErrInvalidPending)
			}
			task := g.addTaskLocked(want.ID)
			want.ID = task.ID
		}
		wanted[want.ID] = want
	}
	tasks := make([]Task, 0, len(wanted))
	for _, task := range g.tasks {
		want, keep := wanted[task.ID]
		if !keep {
			if task.Outcome == OutcomeActive && !g.taskHasHistoryLocked(task.ID) {
				continue
			}
			want = PendingTask{ID: task.ID, Info: task.Info, RunPolicy: task.RunPolicy, Persistent: task.Persistent}
			wanted[task.ID] = want
		}
		if want.Info != "" {
			task.Info = want.Info
		}
		if want.RunPolicy != "" {
			task.RunPolicy = want.RunPolicy
		}
		task.Persistent = task.Persistent || want.Persistent
		tasks = append(tasks, task)
	}
	g.tasks = tasks
	nodes := make([]Node, 0, len(g.nodes))
	for _, node := range g.nodes {
		if _, keep := wanted[node.TaskID]; keep {
			nodes = append(nodes, node)
		} else {
			delete(g.outputs, node.ID)
		}
	}
	g.nodes = nodes
	g.edges = nil
	seen := make(map[Edge]bool)
	for _, task := range tasks {
		for _, edge := range []Edge{{From: task.Planner.ID, To: task.Executor.ID}, {From: task.Executor.ID, To: task.Verifier.ID}} {
			g.edges = append(g.edges, edge)
			seen[edge] = true
		}
	}
	for _, edge := range oldEdges {
		if !g.immutableNodeLocked(edge.To) || seen[edge] {
			continue
		}
		g.edges = append(g.edges, edge)
		seen[edge] = true
	}
	explicit := make(map[Edge]bool, len(next.Edges))
	for _, edge := range next.Edges {
		if explicit[edge] {
			return fmt.Errorf("%w: duplicate edge %q → %q", ErrInvalidPending, edge.From, edge.To)
		}
		explicit[edge] = true
		if seen[edge] {
			continue
		}
		if !oldSet[edge] {
			if err := g.validateSourceLocked(edge.From); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidPending, err)
			}
		}
		g.edges = append(g.edges, edge)
		seen[edge] = true
	}
	return nil
}

func validTaskID(id string) bool {
	return id != "" && strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) < 0
}

func (g *Graph) taskHasHistoryLocked(taskID string) bool {
	for _, output := range g.outputs {
		if output.Node.TaskID == taskID {
			return true
		}
	}
	return false
}

func (g *Graph) immutableNodeLocked(nodeID string) bool {
	if _, ok := g.outputs[nodeID]; ok {
		return true
	}
	node, ok := g.nodeByIDLocked(nodeID)
	if !ok {
		return false
	}
	task, ok := g.taskByIDLocked(node.TaskID)
	if !ok {
		return false
	}
	if task.Outcome != OutcomeActive {
		return true
	}
	return !currentActivationNode(task, node)
}

func immutableHistoryUnchanged(current, next *Graph) error {
	for _, task := range current.tasks {
		frozen := task.Outcome != OutcomeActive
		for _, node := range current.nodes {
			if node.TaskID == task.ID && currentActivationNode(task, node) {
				if _, ready := current.outputs[node.ID]; ready {
					frozen = true
					break
				}
			}
		}
		if !frozen {
			continue
		}
		if candidate, ok := next.taskByIDLocked(task.ID); !ok || candidate != task {
			return fmt.Errorf("%w: %w: task %q already produced output", ErrInvalidPending, ErrGraphBusy, task.ID)
		}
	}
	for _, node := range current.nodes {
		if current.immutableNodeLocked(node.ID) && !sameIncoming(current.edges, next.edges, node.ID) {
			return fmt.Errorf("%w: completed node %q input is immutable", ErrGraphBusy, node.ID)
		}
	}
	return nil
}

func sameIncoming(left, right []Edge, nodeID string) bool {
	incoming := func(edges []Edge) []string {
		var ids []string
		for _, edge := range edges {
			if edge.To == nodeID {
				ids = append(ids, edge.From)
			}
		}
		return ids
	}
	return slices.Equal(incoming(left), incoming(right))
}

func runningSliceUnchanged(current, next *Graph, running *runner) error {
	tasks, nodes := running.executionSnapshot()
	for id := range tasks {
		before, ok := current.taskByIDLocked(id)
		if !ok {
			continue
		}
		if after, ok := next.taskByIDLocked(id); !ok || before != after {
			return fmt.Errorf("%w: task %q already started", ErrGraphBusy, id)
		}
	}
	for id := range nodes {
		if !sameIncoming(current.edges, next.edges, id) {
			return fmt.Errorf("%w: node %q already started", ErrGraphBusy, id)
		}
	}
	return nil
}

func persistedInputsUnchanged(current, next *Graph) error {
	if current.progress == nil {
		return nil
	}
	for _, task := range current.tasks {
		candidate, exists := next.taskByIDLocked(task.ID)
		taskChanged := !exists || candidate != task
		changedInputs := make(map[string]bool)
		for _, node := range current.nodes {
			if node.TaskID == task.ID && !sameIncoming(current.edges, next.edges, node.ID) {
				changedInputs[node.ID] = true
			}
		}
		if !taskChanged && len(changedInputs) == 0 {
			continue
		}
		state, _, err := current.progress.Load(task.Env.ID)
		if err != nil {
			return err
		}
		for _, input := range state.Inputs {
			if taskChanged || changedInputs[input.NodeID] {
				return fmt.Errorf("%w: node %q has a persisted input batch", ErrGraphBusy, input.NodeID)
			}
		}
	}
	return nil
}

func (g *Graph) inputFrozenLocked(nodeID string) (bool, error) {
	if g.immutableNodeLocked(nodeID) {
		return true, nil
	}
	for _, running := range g.runners {
		_, nodes := running.executionSnapshot()
		if _, ok := nodes[nodeID]; ok {
			return true, nil
		}
	}
	if g.progress != nil {
		node, _ := g.nodeByIDLocked(nodeID)
		task, _ := g.taskByIDLocked(node.TaskID)
		state, _, err := g.progress.Load(task.Env.ID)
		if err != nil {
			return false, err
		}
		for _, input := range state.Inputs {
			if input.NodeID == nodeID {
				return true, nil
			}
		}
	}
	return false, nil
}
