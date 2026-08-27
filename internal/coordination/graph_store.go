package coordination

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type graphState struct {
	Revision int64       `json:"revision"`
	NextID   uint64      `json:"next_id"`
	Tasks    []Task      `json:"tasks"`
	Edges    []Edge      `json:"edges"`
	Helps    []helpState `json:"helps,omitempty"`
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
	if state.Revision < 0 {
		return fmt.Errorf("negative revision")
	}
	knownTasks := make(map[string]struct{}, len(state.Tasks))
	knownNodes := make(map[string]struct{}, len(state.Tasks)*3)
	var maxID uint64
	for _, task := range state.Tasks {
		if task.ID == "" {
			return fmt.Errorf("task id is required")
		}
		if _, exists := knownTasks[task.ID]; exists {
			return fmt.Errorf("duplicate task %q", task.ID)
		}
		knownTasks[task.ID] = struct{}{}
		if raw := strings.TrimPrefix(task.ID, "task-"); raw != task.ID {
			id, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid task id %q", task.ID)
			}
			maxID = max(maxID, id)
		}
		for role, node := range map[string]Node{
			RolePlanner: task.Planner, RoleExecutor: task.Executor, RoleVerifier: task.Verifier,
		} {
			if node.ID != task.ID+":"+role || node.TaskID != task.ID || node.Role != role {
				return fmt.Errorf("invalid %s node for task %q", role, task.ID)
			}
			if _, exists := knownNodes[node.ID]; exists {
				return fmt.Errorf("duplicate node %q", node.ID)
			}
			knownNodes[node.ID] = struct{}{}
		}
	}
	if state.NextID < maxID {
		return fmt.Errorf("next id %d precedes task %d", state.NextID, maxID)
	}
	for _, edge := range state.Edges {
		if _, ok := knownNodes[edge.From]; !ok {
			return fmt.Errorf("edge from unknown node %q", edge.From)
		}
		if _, ok := knownNodes[edge.To]; !ok {
			return fmt.Errorf("edge to unknown node %q", edge.To)
		}
		switch edge.Kind {
		case EdgeKindSequence, EdgeKindSpawn, EdgeKindJoin:
		default:
			return fmt.Errorf("invalid edge kind %q", edge.Kind)
		}
	}
	knownHelp := make(map[string]struct{}, len(state.Helps))
	for _, help := range state.Helps {
		if help.ID == "" || help.CallID == "" {
			return fmt.Errorf("help id and call id are required")
		}
		if _, exists := knownHelp[help.ID]; exists {
			return fmt.Errorf("duplicate help request %q", help.ID)
		}
		knownHelp[help.ID] = struct{}{}
		if _, ok := knownNodes[help.NodeID]; !ok {
			return fmt.Errorf("help request %q has unknown node %q", help.ID, help.NodeID)
		}
		for _, child := range help.Children {
			if _, ok := knownNodes[child.From]; !ok {
				return fmt.Errorf("help request %q has unknown source %q", help.ID, child.From)
			}
			if _, ok := knownTasks[child.TaskID]; !ok {
				return fmt.Errorf("help request %q has unknown child %q", help.ID, child.TaskID)
			}
		}
	}
	return nil
}

func (g *Graph) stateLocked() graphState {
	tasks := append([]Task(nil), g.tasks...)
	for i := range tasks {
		tasks[i].SpawnedFrom = ""
		tasks[i].Joins = nil
		tasks[i].JoinedBy = nil
	}
	return graphState{
		Revision: g.revision,
		NextID:   g.nextID,
		Tasks:    tasks,
		Edges:    append([]Edge(nil), g.edges...),
		Helps:    cloneHelpStates(g.helps),
	}
}

func (g *Graph) applyStateLocked(state graphState) {
	g.tasks = append([]Task(nil), state.Tasks...)
	for i := range g.tasks {
		if g.tasks[i].RunPolicy == "" {
			g.tasks[i].RunPolicy = RunPolicyEnabled
		}
	}
	g.edges = append([]Edge(nil), state.Edges...)
	g.nextID = state.NextID
	g.helps = cloneHelpStates(state.Helps)
	g.revision = state.Revision
	g.executing = false
}

func cloneHelpStates(states []helpState) []helpState {
	cloned := append([]helpState(nil), states...)
	for i := range cloned {
		cloned[i].Children = append([]helpChildState(nil), states[i].Children...)
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
		executing := g.executing
		g.applyStateLocked(before)
		g.executing = executing
		return err
	}
	return nil
}
