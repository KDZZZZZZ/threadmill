package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestGraphToolsReplacePendingDiffsSpawns(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	tools := GraphTools(graph)

	if _, err := executeGraphTool(t, tools, coordReplacePendingName, `{"roots":[{"info":"root"}]}`); err != nil {
		t.Fatalf("replacePending root: %v", err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}

	spawnArgs, err := json.Marshal(PendingSubgraph{
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Verifier.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeGraphTool(t, tools, coordReplacePendingName, string(spawnArgs)); err != nil {
		t.Fatalf("replacePending spawn: %v", err)
	}

	againArgs, err := json.Marshal(PendingSubgraph{
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Executor.ID, Join: root.Verifier.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeGraphTool(t, tools, coordReplacePendingName, string(againArgs)); err != nil {
		t.Fatalf("replacePending hot modify: %v", err)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("tasks = %d, want 2", graph.taskCount())
	}
}

func TestGraphToolsReplacePendingRejectsCycle(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	tools := GraphTools(graph)
	if _, err := executeGraphTool(t, tools, coordReplacePendingName, `{"roots":[{}]}`); err != nil {
		t.Fatal(err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}
	args, err := json.Marshal(PendingSubgraph{
		Roots:  []PendingRoot{{}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Planner.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeGraphTool(t, tools, coordReplacePendingName, string(args))
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("error = %v, want %v", err, ErrJoinCycle)
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1", graph.taskCount())
	}
}

func TestGraphToolNilGraph(t *testing.T) {
	t.Parallel()

	_, err := graphTool{name: coordReplacePendingName}.Execute(context.Background(), agenttool.Call{
		ID:   "c1",
		Name: coordReplacePendingName,
	})
	if err == nil || !strings.Contains(err.Error(), "nil graph") {
		t.Fatalf("error = %v, want nil graph", err)
	}
}

func executeGraphTool(t *testing.T, tools []agenttool.Tool, name, arguments string) (agenttool.Output, error) {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name != name {
			continue
		}
		if err := tool.Definition().Validate(); err != nil {
			t.Fatalf("%s definition: %v", name, err)
		}
		return tool.Execute(context.Background(), agenttool.Call{
			ID:        "c-" + name,
			Name:      name,
			Arguments: json.RawMessage(arguments),
		})
	}
	t.Fatalf("tool %q not found", name)
	return agenttool.Output{}, nil
}
