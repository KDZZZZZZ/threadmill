package coordination

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const graphStateVersion = 1

// ErrGraphStateVersion rejects legacy graph formats and unsupported future formats.
var ErrGraphStateVersion = errors.New("coordination: unsupported graph state version")

type graphState struct {
	Version         int              `json:"version"`
	Nodes           []Node           `json:"nodes"`
	Outputs         []Output         `json:"outputs"`
	Revision        int64            `json:"revision"`
	NextID          uint64           `json:"next_id"`
	ProjectTaskID   string           `json:"project_task_id,omitempty"`
	ProjectMessages []ProjectMessage `json:"project_messages,omitempty"`
	Tasks           []Task           `json:"tasks"`
	Edges           []Edge           `json:"edges"`
	Helps           []helpState      `json:"helps,omitempty"`
}

// OpenGraph 打开一张持久化协调图；文件不存在时创建空图。
func OpenGraph(path string) (*Graph, error) {
	if path == "" {
		return nil, fmt.Errorf("coordination: graph state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create graph state directory: %w", err)
	}

	g := New()
	g.statePath = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read graph state %q: %w", path, err)
		}
		if err := g.saveLocked(); err != nil {
			return nil, err
		}
		return g, nil
	}
	var state graphState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode graph state %q: %w", path, err)
	}
	if err := validateGraphState(state); err != nil {
		return nil, fmt.Errorf("decode graph state %q: %w", path, err)
	}
	g.applyStateLocked(state)
	return g, nil
}

func validateGraphState(state graphState) error {
	if state.Version != graphStateVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrGraphStateVersion, state.Version, graphStateVersion)
	}
	if state.Revision < 0 {
		return fmt.Errorf("negative revision")
	}
	tasks := make(map[string]Task, len(state.Tasks))
	envs := make(map[string]bool, len(state.Tasks))
	var maxID uint64
	for _, task := range state.Tasks {
		if !validTaskID(task.ID) {
			return fmt.Errorf("invalid task ID %q", task.ID)
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("duplicate task %q", task.ID)
		}
		if task.Activation == 0 {
			return fmt.Errorf("task %q has no activation", task.ID)
		}
		if !validTaskID(task.Env.ID) || envs[task.Env.ID] {
			return fmt.Errorf("invalid or duplicate environment %q", task.Env.ID)
		}
		if task.RunPolicy != RunPolicyEnabled && task.RunPolicy != RunPolicyHeld {
			return fmt.Errorf("task %q has invalid run policy %q", task.ID, task.RunPolicy)
		}
		switch task.Outcome {
		case OutcomeActive, OutcomeDone, OutcomeCanceled, OutcomeFailed:
		case OutcomeIdle, OutcomeClosed:
			if !task.Persistent {
				return fmt.Errorf("nonpersistent task %q has outcome %q", task.ID, task.Outcome)
			}
		default:
			return fmt.Errorf("task %q has invalid outcome %q", task.ID, task.Outcome)
		}
		tasks[task.ID] = task
		envs[task.Env.ID] = true
		if raw, ok := strings.CutPrefix(task.ID, "task-"); ok {
			if id, err := strconv.ParseUint(raw, 10, 64); err == nil {
				maxID = max(maxID, id)
			}
		}
	}
	if state.NextID < maxID {
		return fmt.Errorf("next ID %d precedes task %d", state.NextID, maxID)
	}
	nodes := make(map[string]Node, len(state.Nodes))
	for _, node := range state.Nodes {
		task, ok := tasks[node.TaskID]
		if !ok {
			return fmt.Errorf("node %q has unknown task %q", node.ID, node.TaskID)
		}
		if !validNodeForTask(task, node) {
			return fmt.Errorf("invalid node %q", node.ID)
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("duplicate node %q", node.ID)
		}
		nodes[node.ID] = node
	}
	for _, task := range state.Tasks {
		for role, node := range map[string]Node{RolePlanner: task.Planner, RoleExecutor: task.Executor, RoleVerifier: task.Verifier} {
			if node.ID != fmt.Sprintf("%s:%d:%s", task.ID, task.Activation, role) || node.TaskID != task.ID || node.Role != role || nodes[node.ID] != node {
				return fmt.Errorf("invalid %s node for task %q", role, task.ID)
			}
		}
	}
	outputs := make(map[string]Output, len(state.Outputs))
	for _, output := range state.Outputs {
		if node, ok := nodes[output.Node.ID]; !ok || node != output.Node {
			return fmt.Errorf("output has unknown node %q", output.Node.ID)
		}
		if output.FilesRef == "" || output.MemoryRef == "" {
			return fmt.Errorf("output %q requires paired file and memory references", output.Node.ID)
		}
		if _, exists := outputs[output.Node.ID]; exists {
			return fmt.Errorf("duplicate output %q", output.Node.ID)
		}
		outputs[output.Node.ID] = output
	}
	edges := make(map[Edge]bool, len(state.Edges))
	successors := make(map[string][]string)
	indegree := make(map[string]int, len(nodes))
	for _, edge := range state.Edges {
		if _, ok := nodes[edge.From]; !ok {
			return fmt.Errorf("edge from unknown node %q", edge.From)
		}
		if _, ok := nodes[edge.To]; !ok {
			return fmt.Errorf("edge to unknown node %q", edge.To)
		}
		if edges[edge] {
			return fmt.Errorf("duplicate edge %q → %q", edge.From, edge.To)
		}
		edges[edge] = true
		successors[edge.From] = append(successors[edge.From], edge.To)
		indegree[edge.To]++
	}
	queue := make([]string, 0, len(nodes))
	for id := range nodes {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, id := range successors[queue[i]] {
			indegree[id]--
			if indegree[id] == 0 {
				queue = append(queue, id)
			}
		}
	}
	if len(queue) != len(nodes) {
		return ErrCycle
	}
	for _, task := range state.Tasks {
		for _, edge := range []Edge{{From: task.Planner.ID, To: task.Executor.ID}, {From: task.Executor.ID, To: task.Verifier.ID}} {
			if !edges[edge] {
				return fmt.Errorf("task %q is missing role sequence edge", task.ID)
			}
		}
	}
	if state.ProjectTaskID != "" {
		owner, exists := tasks[state.ProjectTaskID]
		if !exists || !owner.RealDirectory {
			return fmt.Errorf("invalid real directory task %q", state.ProjectTaskID)
		}
	}
	messageIDs := make(map[string]bool, len(state.ProjectMessages))
	for _, message := range state.ProjectMessages {
		node, exists := nodes[message.NodeID]
		if !exists || !tasks[node.TaskID].RealDirectory || message.ID == "" || messageIDs[message.ID] || message.Content == "" ||
			(message.From != ManagerEnvID && message.From != node.ID) {
			return fmt.Errorf("invalid project message %q", message.ID)
		}
		messageIDs[message.ID] = true
	}
	helps := make(map[string]bool, len(state.Helps))
	for _, help := range state.Helps {
		if help.ID == "" || help.CallID == "" {
			return fmt.Errorf("help ID and call ID are required")
		}
		if helps[help.ID] {
			return fmt.Errorf("duplicate help request %q", help.ID)
		}
		helps[help.ID] = true
		for _, id := range []string{help.NodeID, help.PauseID, help.ResumeID} {
			if _, ok := nodes[id]; !ok {
				return fmt.Errorf("help request %q has unknown node %q", help.ID, id)
			}
		}
		if help.Units != nil {
			if err := validateHelpUnits(help.Units); err != nil {
				return fmt.Errorf("help request %q: %w", help.ID, err)
			}
		}
		for _, id := range help.TaskIDs {
			if _, ok := tasks[id]; !ok {
				return fmt.Errorf("help request %q has unknown task %q", help.ID, id)
			}
		}
	}
	return nil
}

func (g *Graph) stateLocked() graphState {
	return graphState{
		Version:         graphStateVersion,
		Nodes:           append([]Node(nil), g.nodes...),
		Outputs:         g.outputListLocked(),
		Revision:        g.revision,
		NextID:          g.nextID,
		ProjectTaskID:   g.projectTaskID,
		ProjectMessages: append([]ProjectMessage(nil), g.projectMessages...),
		Tasks:           append([]Task(nil), g.tasks...),
		Edges:           append([]Edge(nil), g.edges...),
		Helps:           cloneHelpStates(g.helps),
	}
}

func (g *Graph) applyStateLocked(state graphState) {
	g.tasks = append([]Task(nil), state.Tasks...)
	g.nodes = append([]Node(nil), state.Nodes...)
	g.outputs = make(map[string]Output, len(state.Outputs))
	for _, output := range state.Outputs {
		g.outputs[output.Node.ID] = output
	}
	g.edges = append([]Edge(nil), state.Edges...)
	g.nextID = state.NextID
	g.helps = cloneHelpStates(state.Helps)
	g.revision = state.Revision
	g.projectTaskID = state.ProjectTaskID
	g.projectMessages = append([]ProjectMessage(nil), state.ProjectMessages...)
}

func cloneHelpStates(states []helpState) []helpState {
	cloned := append([]helpState(nil), states...)
	for i := range cloned {
		cloned[i].Units = cloneHelpUnits(states[i].Units)
		cloned[i].TaskIDs = append([]string(nil), states[i].TaskIDs...)
	}
	return cloned
}

func (g *Graph) saveLocked() error {
	if g.statePath == "" {
		return nil
	}
	data, err := json.Marshal(g.stateLocked())
	if err != nil {
		return fmt.Errorf("encode graph state: %w", err)
	}
	tmp := g.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write graph state %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, g.statePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit graph state %q: %w", g.statePath, err)
	}
	return nil
}

func (g *Graph) saveOrRestoreLocked(before graphState) error {
	if err := g.saveLocked(); err != nil {
		g.applyStateLocked(before)
		return err
	}
	g.notifyLocked()
	return nil
}

func (g *Graph) saveAndProjectLocked(before graphState) error {
	if err := g.saveLocked(); err != nil {
		g.applyStateLocked(before)
		return err
	}
	if err := emitTasks(g.taskSink, g.snapshotLocked().Tasks); err != nil {
		g.applyStateLocked(before)
		return errors.Join(err, g.saveLocked())
	}
	g.notifyLocked()
	return nil
}

func (g *Graph) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
	}
	g.changed = make(chan struct{})
}
