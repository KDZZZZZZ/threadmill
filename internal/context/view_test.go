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

func TestGlobalViewWritesGlobalGraphNotStore(t *testing.T) {
	t.Cleanup(func() { Update(Copy{}) })
	Update(Copy{})

	store := NewStore()
	envView := store.View("env-1")
	global := GlobalView("agent-a")

	global.Commit(Graph{
		Nodes: []Node{{ID: "g1", Statement: "global"}},
	})
	envView.Commit(Graph{
		Nodes: []Node{{ID: "e1", Statement: "env"}},
	})

	gotGlobal := global.Snapshot()
	if len(gotGlobal.Nodes) != 1 || gotGlobal.Nodes[0].ID != "g1" {
		t.Fatalf("GlobalView snapshot = %#v, want g1", gotGlobal.Nodes)
	}
	gotEnv := envView.Snapshot()
	if len(gotEnv.Nodes) != 1 || gotEnv.Nodes[0].ID != "e1" {
		t.Fatalf("Store.View snapshot = %#v, want e1", gotEnv.Nodes)
	}

	if nodes := Clone("check").Graph.Nodes; len(nodes) != 1 || nodes[0].ID != "g1" {
		t.Fatalf("global graph = %#v, want g1", nodes)
	}
	if nodes := store.Load("env-1").Nodes; len(nodes) != 1 || nodes[0].ID != "e1" {
		t.Fatalf("store env-1 = %#v, want e1", nodes)
	}
}
