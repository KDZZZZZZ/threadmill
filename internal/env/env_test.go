package env

import (
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestOpenSetsIDAndMemory(t *testing.T) {
	t.Parallel()

	view := stubMemory{}
	got := Open("env-a", view)
	if got.ID != "env-a" {
		t.Fatalf("ID = %q, want env-a", got.ID)
	}
	if got.Memory != view {
		t.Fatal("Memory was not the view passed to Open")
	}
}

type stubMemory struct{}

func (stubMemory) Snapshot() ctxgraph.Graph { return ctxgraph.Graph{} }

func (stubMemory) Commit(ctxgraph.Graph) {}
