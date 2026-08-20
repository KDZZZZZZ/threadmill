package coordination

import (
	"context"
	"errors"
	"reflect"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
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

func TestTaskSinkReceivesExistingAndUpdatedTaskInfo(t *testing.T) {
	graph := newGraph()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "initial"}},
	}); err != nil {
		t.Fatal(err)
	}

	var got []Task
	if err := graph.SetTaskSink(func(tasks []Task) error {
		got = append(got, tasks...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "updated"}},
	}); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Info != "initial" || got[1].Info != "updated" {
		t.Fatalf("sink tasks = %#v, want initial then updated task info", got)
	}
}

func TestReplacePendingRetriesFailedTaskSinkWithoutDuplicatingTasks(t *testing.T) {
	path := t.TempDir() + "/graph.json"
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	sinkErr := errors.New("sink failed")
	fail := true
	if err := graph.SetTaskSink(func([]Task) error {
		if fail {
			return sinkErr
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := PendingSubgraph{Roots: []PendingRoot{{Info: "root"}}}
	if _, err := graph.ReplacePending(context.Background(), want); !errors.Is(err, sinkErr) {
		t.Fatalf("first ReplacePending() error = %v, want sink error", err)
	}
	if got := graph.taskCount(); got != 0 {
		t.Fatalf("tasks after failed sink = %d, want graph unchanged", got)
	}
	restored, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.taskCount(); got != 0 {
		t.Fatalf("persisted tasks after failed sink = %d, want graph unchanged", got)
	}

	fail = false
	if _, err := graph.ReplacePending(context.Background(), want); err != nil {
		t.Fatalf("retry ReplacePending() error = %v", err)
	}
	if got := graph.taskCount(); got != 1 {
		t.Fatalf("tasks after retry = %d, want no duplicate root", got)
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

func TestReplacePendingKeepsSiblingSpawnsWithSameEndpoints(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "integrate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := snap.Tasks[0]
	want := PendingSubgraph{
		Roots: []PendingRoot{{Info: "integrate"}},
		Spawns: []PendingSpawn{
			{From: root.Planner.ID, Join: root.Executor.ID, Info: "pricing"},
			{From: root.Planner.ID, Join: root.Executor.ID, Info: "inventory"},
			{From: root.Planner.ID, Join: root.Executor.ID, Info: "shipping"},
		},
	}
	if _, err := graph.ReplacePending(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got := graph.taskCount(); got != 4 {
		t.Fatalf("tasks = %d, want one root and three siblings", got)
	}
	if _, err := graph.ReplacePending(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got := graph.taskCount(); got != 4 {
		t.Fatalf("tasks after identical replace = %d, want no duplicates", got)
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

func TestReplacePendingRejectsCompletedTaskChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		next func(Task, PendingSpawn) PendingSubgraph
	}{
		{
			name: "root info",
			next: func(_ Task, spawn PendingSpawn) PendingSubgraph {
				return PendingSubgraph{
					Roots:  []PendingRoot{{Info: "changed root"}},
					Spawns: []PendingSpawn{spawn},
				}
			},
		},
		{
			name: "child info",
			next: func(_ Task, spawn PendingSpawn) PendingSubgraph {
				spawn.Info = "changed child"
				return PendingSubgraph{
					Roots:  []PendingRoot{{Info: "root"}},
					Spawns: []PendingSpawn{spawn},
				}
			},
		},
		{
			name: "remove child",
			next: func(_ Task, _ PendingSpawn) PendingSubgraph {
				return PendingSubgraph{Roots: []PendingRoot{{Info: "root"}}}
			},
		},
		{
			name: "move child",
			next: func(root Task, spawn PendingSpawn) PendingSubgraph {
				spawn.From = root.Executor.ID
				return PendingSubgraph{
					Roots:  []PendingRoot{{Info: "root"}},
					Spawns: []PendingSpawn{spawn},
				}
			},
		},
		{
			name: "add child",
			next: func(root Task, spawn PendingSpawn) PendingSubgraph {
				return PendingSubgraph{
					Roots: []PendingRoot{{Info: "root"}},
					Spawns: []PendingSpawn{
						spawn,
						{From: root.Executor.ID, Join: root.Verifier.ID, Info: "new child"},
					},
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graph := newGraph()
			snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
				Roots: []PendingRoot{{Info: "root"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			root := snap.Tasks[0]
			spawn := PendingSpawn{From: root.Planner.ID, Join: root.Verifier.ID, Info: "child"}
			if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
				Roots: []PendingRoot{{Info: "root"}}, Spawns: []PendingSpawn{spawn},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := graph.Run(
				context.Background(), root.ID, "input", Stores{Memory: ctxgraph.NewStore()}, recordingAssemble(nil),
			); err != nil {
				t.Fatal(err)
			}
			before := graph.Snapshot()

			_, err = graph.ReplacePending(context.Background(), tt.next(root, spawn))
			if !errors.Is(err, ErrInvalidPending) {
				t.Fatalf("ReplacePending() error = %v, want %v", err, ErrInvalidPending)
			}
			if after := graph.Snapshot(); !reflect.DeepEqual(after, before) {
				t.Fatalf("completed graph changed:\nafter  = %#v\nbefore = %#v", after, before)
			}
		})
	}
}

func TestReplacePendingCanAddRootBesideCompletedGraph(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Run(
		context.Background(), snap.Tasks[0].ID, "input", Stores{Memory: ctxgraph.NewStore()}, recordingAssemble(nil),
	); err != nil {
		t.Fatal(err)
	}

	got, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "done"}, {Info: "new"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 2 || got.Tasks[0].Outcome != OutcomeDone || got.Tasks[1].Outcome != OutcomeActive {
		t.Fatalf("tasks = %#v, want immutable done root plus active root", got.Tasks)
	}
}
