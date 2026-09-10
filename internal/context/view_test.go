package context

import "testing"

func TestStoreViewsStayIsolated(t *testing.T) {
	t.Parallel()

	store := NewStore()
	viewA := store.View("env-a")
	viewB := store.View("env-b")

	viewA.Commit(Graph{
		Nodes: []Node{{ID: "n1", Statement: "secret"}},
	})

	if nodes := viewB.Snapshot().Nodes; len(nodes) != 0 {
		t.Fatalf("env-b snapshot = %#v, want empty", nodes)
	}

	viewB.Commit(Graph{
		Nodes: []Node{{ID: "n1", Statement: "other"}},
	})

	gotA := viewA.Snapshot()
	if len(gotA.Nodes) != 1 || gotA.Nodes[0].Statement != "secret" {
		t.Fatalf("env-a snapshot = %#v, want secret", gotA.Nodes)
	}
	gotB := viewB.Snapshot()
	if len(gotB.Nodes) != 1 || gotB.Nodes[0].Statement != "other" {
		t.Fatalf("env-b snapshot = %#v, want other", gotB.Nodes)
	}

	gotA.Nodes[0].Statement = "mutated"
	if store.Load("env-a").Nodes[0].Statement != "secret" {
		t.Fatal("mutating Snapshot changed the store")
	}
}
