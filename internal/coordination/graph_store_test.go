package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestOpenGraphRejectsUnsupportedSchemaVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []int{0, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "graph.json")
			data, err := json.Marshal(map[string]any{"version": version, "revision": 0, "tasks": []any{}, "edges": []any{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenGraph(path); !errors.Is(err, ErrGraphStateVersion) {
				t.Fatalf("OpenGraph(version %d) = %v", version, err)
			}
		})
	}
}

func TestOpenGraphRejectsCorruptUnifiedState(t *testing.T) {
	t.Parallel()
	graph := New()
	task := graph.AddTask()
	if err := graph.commitOutput(Output{Node: task.Planner, FilesRef: "files", MemoryRef: "memory"}); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(graph.stateLocked())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*graphState){
		"unpaired output":        func(s *graphState) { s.Outputs[0].MemoryRef = "" },
		"unknown output node":    func(s *graphState) { s.Outputs[0].Node.ID = "missing" },
		"duplicate output":       func(s *graphState) { s.Outputs = append(s.Outputs, s.Outputs[0]) },
		"node identity mismatch": func(s *graphState) { s.Nodes[0].Role = RoleVerifier },
		"unknown checkpoint task": func(s *graphState) {
			s.Nodes = append(s.Nodes, Node{ID: "missing:1:planner:pause", TaskID: "missing", Role: RolePlanner})
		},
		"activation zero":    func(s *graphState) { s.Tasks[0].Activation = 0 },
		"empty environment":  func(s *graphState) { s.Tasks[0].Env.ID = "" },
		"unsafe environment": func(s *graphState) { s.Tasks[0].Env.ID = "../project" },
		"invalid outcome":    func(s *graphState) { s.Tasks[0].Outcome = "root" },
		"missing run policy": func(s *graphState) { s.Tasks[0].RunPolicy = "" },
		"cycle":              func(s *graphState) { s.Edges = append(s.Edges, Edge{From: task.Verifier.ID, To: task.Planner.ID}) },
		"duplicate edge":     func(s *graphState) { s.Edges = append(s.Edges, s.Edges[0]) },
		"missing role chain": func(s *graphState) { s.Edges = nil },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var state graphState
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			corrupt(&state)
			invalid, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "graph.json")
			if err := os.WriteFile(path, invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenGraph(path); err == nil {
				t.Fatal("accepted corrupt graph state")
			}
		})
	}
}

func TestOpenGraphPreservesNamedAndAllocatedTaskIDs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "task-40", Info: "named numeric task"}, {ID: "task-manual", Info: "named ordinary task"}}})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if next := reopened.AddTask(); next.ID != "task-41" {
		t.Fatalf("allocated task = %s, want task-41", next.ID)
	}
}

func TestFailedGraphSaveRestoresStateAndKeepsRunner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "watch", Info: "observe", Persistent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	task := snap.Tasks[0]
	canceled := false
	running := &runner{task: task, cancel: func() { canceled = true }, nodeStarted: map[string]struct{}{}}
	graph.runners[task.ID] = running
	before := graph.Snapshot()
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := graph.commitOutput(Output{Node: task.Planner, FilesRef: "files", MemoryRef: "memory"}); err == nil {
		t.Fatal("expected failed output save")
	}
	if err := graph.CloseTask(task.ID); err == nil {
		t.Fatal("expected failed close save")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) || graph.runners[task.ID] != running || canceled {
		t.Fatal("failed save changed state or canceled live activation")
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if restored, _ := reopened.Task(task.ID); restored != task {
		t.Fatal("failed save changed durable task")
	}
	if _, ok := reopened.Output(task.Planner.ID); ok {
		t.Fatal("failed output save became visible on restart")
	}
}
