package coordination

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestOpenGraphRestoresStateAndContinuesTaskIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	first, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := first.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "first"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	second, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored snapshot = %#v, want %#v", got, want)
	}

	got, err := second.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "first"}, {Info: "second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 2 || got.Tasks[1].ID != "task-2" {
		t.Fatalf("tasks = %#v, want restored task-1 followed by task-2", got.Tasks)
	}
}

func TestOpenGraphRestoresRunOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	first, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := first.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "finish"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(
		context.Background(),
		snap.Tasks[0].ID,
		"input",
		Stores{Memory: ctxgraph.NewStore()},
		func(Task) (Roles, error) { return instantRoles(), nil },
	); err != nil {
		t.Fatal(err)
	}

	second, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	got := second.Snapshot()
	if len(got.Tasks) != 1 || got.Tasks[0].Outcome != OutcomeDone {
		t.Fatalf("restored tasks = %#v, want one done task", got.Tasks)
	}
}
