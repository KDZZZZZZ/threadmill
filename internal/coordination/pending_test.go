package coordination

import (
	"context"
	"errors"
	"testing"
)

func TestReplacePendingCreatesRoot(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "ship the cli"}},
	})
	if err != nil {
		t.Fatalf("ReplacePending() error = %v", err)
	}
	if snap.Revision != 1 || len(snap.Tasks) != 1 {
		t.Fatalf("snapshot = %+v, want revision 1 and 1 task", snap)
	}
	if snap.Tasks[0].Info != "ship the cli" {
		t.Fatalf("root info = %q, want ship the cli", snap.Tasks[0].Info)
	}
}

func TestReplacePendingDiffsSpawns(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "root"}},
	}); err != nil {
		t.Fatal(err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}

	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Verifier.ID, Info: "child"}},
	}); err != nil {
		t.Fatalf("spawn via diff: %v", err)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("tasks = %d, want 2", graph.taskCount())
	}

	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Executor.ID, Join: root.Verifier.ID, Info: "child"}},
	}); err != nil {
		t.Fatalf("hot modify via diff: %v", err)
	}
	parent, ok := graph.Task(root.ID)
	if !ok || len(parent.JoinedBy) != 1 {
		t.Fatalf("JoinedBy = %v, want one child", parent.JoinedBy)
	}
	child, ok := graph.Task(parent.JoinedBy[0])
	if !ok {
		t.Fatal("child missing")
	}
	pair, ok := graph.spawnPairLocked(child)
	if !ok || pair.From != root.Executor.ID || pair.Join != root.Verifier.ID {
		t.Fatalf("spawn pair = %+v, want executor->verifier", pair)
	}
}

func TestReplacePendingRejectsCycleWithoutMutating(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{}},
	}); err != nil {
		t.Fatal(err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}

	_, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots:  []PendingRoot{{}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Planner.ID}},
	})
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("error = %v, want %v", err, ErrJoinCycle)
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1 after rejected cycle", graph.taskCount())
	}
}

func TestReplacePendingCannotRemoveRoots(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{}, {}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{}},
	})
	if !errors.Is(err, ErrUnspawnRoot) {
		t.Fatalf("error = %v, want %v", err, ErrUnspawnRoot)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("tasks = %d, want 2 after rejected shrink", graph.taskCount())
	}
}

func TestReplacePendingUpdatesExistingInfo(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "old goal"}},
	}); err != nil {
		t.Fatal(err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "new goal"}},
		Spawns: []PendingSpawn{{
			From: root.Planner.ID,
			Join: root.Verifier.ID,
			Info: "old child",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "new goal"}},
		Spawns: []PendingSpawn{{
			From: root.Planner.ID,
			Join: root.Verifier.ID,
			Info: "new child",
		}},
	})
	if err != nil {
		t.Fatalf("info-only update: %v", err)
	}
	if snap.Revision < 3 {
		t.Fatalf("revision = %d, want bump after info update", snap.Revision)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("tasks = %d, want 2", graph.taskCount())
	}
	root, ok = graph.Task("task-1")
	if !ok || root.Info != "new goal" {
		t.Fatalf("root info = %+v, want new goal", root)
	}
	child, ok := graph.Task(root.JoinedBy[0])
	if !ok || child.Info != "new child" {
		t.Fatalf("child info = %+v, want new child", child)
	}
}
