package context

import "testing"

func TestStoreLoadMissingIsEmpty(t *testing.T) {
	t.Parallel()

	store := NewStore()
	got := store.Load("env-missing")
	if len(got.Nodes) != 0 || len(got.Edges) != 0 || len(got.Subgraphs) != 0 || got.Revision != 0 {
		t.Fatalf("Load missing = %#v, want empty graph", got)
	}
}

func TestStoreSaveLoadDoesNotShareBackingData(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("env-1", Graph{
		Nodes: []Node{{ID: "n1", Statement: "secret"}},
	})

	loaded := store.Load("env-1")
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].Statement != "secret" {
		t.Fatalf("Load = %#v, want secret", loaded)
	}

	loaded.Nodes[0].Statement = "mutated"
	if store.Load("env-1").Nodes[0].Statement != "secret" {
		t.Fatal("mutating Load result changed the store")
	}
}

func TestStoreForkCopiesParentThenIsolatesWrites(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Nodes: []Node{{ID: "n1", Statement: "from-parent"}},
	})
	store.Fork("parent", "child")

	if got := store.Load("child"); len(got.Nodes) != 1 || got.Nodes[0].Statement != "from-parent" {
		t.Fatalf("forked child = %#v, want from-parent", got)
	}

	store.Save("child", Graph{
		Nodes: []Node{{ID: "n1", Statement: "from-child"}},
	})
	if store.Load("parent").Nodes[0].Statement != "from-parent" {
		t.Fatal("child write leaked into parent")
	}
	if store.Load("child").Nodes[0].Statement != "from-child" {
		t.Fatal("child write did not stay in child")
	}
}

func TestStoreForkDoesNotOverwriteExistingChild(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Nodes: []Node{{ID: "n1", Statement: "parent"}},
	})
	store.Save("child", Graph{
		Nodes: []Node{{ID: "n1", Statement: "existing"}},
	})
	store.Fork("parent", "child")

	if store.Load("child").Nodes[0].Statement != "existing" {
		t.Fatal("fork overwrote an existing child snapshot")
	}
}
