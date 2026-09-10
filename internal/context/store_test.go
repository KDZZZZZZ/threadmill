package context

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreRestoreUsesExactImmutableSnapshot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "memory.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := store.Snapshot("missing"); exists {
		t.Fatal("unknown snapshot reported as a valid empty graph")
	}
	if err := store.SaveSnapshot("empty", Graph{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := store.Snapshot("empty"); !exists {
		t.Fatal("valid empty snapshot reported as unknown")
	}
	old := Subgraph{ID: "old-package", Kind: SubgraphKindPackage}
	if err := store.AppendNode("target", old, Node{ID: "old", Statement: "old candidate"}); err != nil {
		t.Fatal(err)
	}
	want := Graph{
		Revision:  7,
		Subgraphs: []Subgraph{{ID: "new-package", Kind: SubgraphKindPackage}},
		Nodes:     []Node{{ID: "new", Statement: "accepted input", SubgraphIDs: []string{"new-package"}}},
	}
	if err := store.SaveSnapshot("ready", want); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot("ready", want); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	if err := store.Restore("target", "ready"); err != nil {
		t.Fatal(err)
	}
	if got := store.Load("target"); !reflect.DeepEqual(got, want.Clone()) {
		t.Fatalf("restore retained old managed memory: %#v", got)
	}
	if err := store.AppendNode("target", Subgraph{ID: "new-package"}, Node{ID: "new", Statement: "later work"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Load("target"); len(got.Nodes) != 1 || got.Nodes[0].Statement != "later work" {
		t.Fatalf("live environment edits were lost on restart: %#v", got)
	}
	if err := reopened.Restore("other-target", "ready"); err != nil {
		t.Fatal(err)
	}
	if got := reopened.Load("other-target"); !reflect.DeepEqual(got, want.Clone()) {
		t.Fatalf("source snapshot followed target edits or was lost on restart: %#v", got)
	}
	if err := reopened.Restore("target", "unknown"); err == nil {
		t.Fatal("restore accepted an unknown snapshot")
	}
}

func TestStoreImmutableSnapshotsRejectEveryWritePath(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		write func(*Store) error
	}{
		{name: "normal commit", write: func(store *Store) error { return store.View("fixed").Commit(Graph{}) }},
		{name: "runtime append", write: func(store *Store) error {
			return store.AppendNode("fixed", Subgraph{ID: "knowledge"}, Node{ID: "new", Statement: "new"})
		}},
		{name: "drop subgraph", write: func(store *Store) error { return store.DropSubgraph("fixed", "knowledge") }},
		{name: "snapshot overwrite", write: func(store *Store) error { return store.SaveSnapshot("fixed", Graph{}) }},
		{name: "restore another snapshot", write: func(store *Store) error { return store.Restore("fixed", "empty") }},
		{name: "restore identical distinct snapshot", write: func(store *Store) error {
			if err := store.SaveSnapshot("duplicate", store.Load("fixed")); err != nil {
				return err
			}
			return store.Restore("fixed", "duplicate")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory.json")
			store, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			original := Graph{
				Subgraphs: []Subgraph{{ID: "knowledge"}},
				Nodes:     []Node{{ID: "n", Statement: "fixed memory", SubgraphIDs: []string{"knowledge"}}},
			}
			if err := store.SaveSnapshot("fixed", original); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveSnapshot("empty", Graph{}); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.write(reopened); err == nil {
				t.Fatal("write changed an immutable source")
			}
			if got := reopened.Load("fixed"); !reflect.DeepEqual(got, original.Clone()) {
				t.Fatalf("failed write changed source: %#v", got)
			}
		})
	}
}

func TestStoreSnapshotPersistenceFailureDoesNotPublishOrReplaceState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "memory.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	original := Graph{Nodes: []Node{{ID: "old", Statement: "old state"}}}
	if err := store.Save("target", original); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot("ready", Graph{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot("uncommitted", Graph{}); err == nil {
		t.Fatal("expected snapshot persistence failure")
	}
	if _, exists := store.Snapshot("uncommitted"); exists {
		t.Fatal("failed snapshot became visible")
	}
	if err := store.Restore("target", "ready"); err == nil {
		t.Fatal("expected restore persistence failure")
	}
	if got := store.Load("target"); !reflect.DeepEqual(got, original.Clone()) {
		t.Fatalf("failed restore changed target: %#v", got)
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot("uncommitted", Graph{}); err != nil {
		t.Fatalf("snapshot retry: %v", err)
	}
	if err := store.Restore("target", "ready"); err != nil {
		t.Fatalf("restore retry: %v", err)
	}
}

func TestStoreLoadMissingIsEmpty(t *testing.T) {
	t.Parallel()

	store := NewStore()
	got := store.Load("env-missing")
	if len(got.Nodes) != 0 || len(got.Edges) != 0 || len(got.Subgraphs) != 0 || got.Revision != 0 {
		t.Fatalf("Load missing = %#v, want empty graph", got)
	}
}

func TestStoreSaveFailureKeepsPreviousSnapshot(t *testing.T) {
	store := NewStore()
	if err := store.Save("env", Graph{Nodes: []Node{{ID: "old", Statement: "old"}}}); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(t.TempDir(), "missing", "memory.json")

	err := store.Save("env", Graph{Nodes: []Node{{ID: "new", Statement: "new"}}})
	if err == nil {
		t.Fatal("Save() error = nil, want persistence error")
	}
	got := store.Load("env")
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "old" {
		t.Fatalf("Load() after failed Save = %#v, want previous snapshot", got.Nodes)
	}
}

func TestStoreRestoreFailureDoesNotCreateTarget(t *testing.T) {
	store := NewStore()
	if err := store.SaveSnapshot("ready", Graph{Nodes: []Node{{ID: "fact", Statement: "fact"}}}); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(t.TempDir(), "missing", "memory.json")

	err := store.Restore("target", "ready")
	if err == nil {
		t.Fatal("Restore() error = nil, want persistence error")
	}
	if _, exists := store.Snapshot("target"); exists {
		t.Fatal("failed Restore created a target environment")
	}
}

func TestStoreAppendsSystemNodesIdempotently(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "memory.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	subgraph := Subgraph{ID: "system", Kind: SubgraphKindSystem}
	if err := store.EnsureSubgraph("env", subgraph); err != nil {
		t.Fatal(err)
	}
	stale := store.Load("env")
	node := Node{ID: "report-1", Statement: "report", Kind: NodeKindFact}
	if err := store.AppendNode("env", subgraph, node); err != nil {
		t.Fatal(err)
	}
	store.Save("env", stale)
	if nodes := store.Load("env").NodesInSubgraphs([]string{subgraph.ID}); len(nodes) != 1 || nodes[0].Statement != "report" {
		t.Fatalf("stale Save removed system report: %#v", nodes)
	}
	if err := store.AppendNode("env", subgraph, node); err != nil {
		t.Fatal(err)
	}
	node.Statement = "updated report"
	if err := store.AppendNode("env", subgraph, node); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendNode("env", subgraph, Node{Statement: "user message"}); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	graph := restored.Load("env")
	if len(graph.Subgraphs) != 1 || graph.Subgraphs[0].Kind != SubgraphKindSystem {
		t.Fatalf("subgraphs = %#v, want one system subgraph", graph.Subgraphs)
	}
	nodes := graph.NodesInSubgraphs([]string{subgraph.ID})
	if len(nodes) != 2 || nodes[0].Statement != "updated report" || nodes[1].Statement != "user message" {
		t.Fatalf("nodes = %#v, want one updated report and one allocated message", nodes)
	}
}

func TestStoreKeepsSystemMembershipWhenAddingNodeToAnotherSubgraph(t *testing.T) {
	t.Parallel()

	store := NewStore()
	system := Subgraph{ID: "system", Kind: SubgraphKindSystem}
	if err := store.AppendNode("env", system, Node{ID: "task-info", Statement: "Task Info"}); err != nil {
		t.Fatal(err)
	}
	graph := store.Load("env").WithSubgraph(Subgraph{ID: "package", Kind: SubgraphKindTask})
	store.Save("env", graph.WithNodesInSubgraph("package", []string{"task-info"}))

	got := store.Load("env").SubgraphsOf("task-info")
	if len(got) != 2 || got[0] != "system" || got[1] != "package" {
		t.Fatalf("memberships = %q, want original system plus package", got)
	}
}

func TestStoreStaleSaveDoesNotRemoveTaskPackageReport(t *testing.T) {
	t.Parallel()

	store := NewStore()
	pack := Subgraph{ID: "task-1-package", Kind: SubgraphKindPackage}
	if err := store.EnsureSubgraph("task", pack); err != nil {
		t.Fatal(err)
	}
	stale := store.Load("task")
	if err := store.AppendNode("task", pack, Node{ID: "report", Statement: "joined report"}); err != nil {
		t.Fatal(err)
	}
	store.Save("task", stale)

	nodes := store.Load("task").NodesInSubgraphs([]string{pack.ID})
	if len(nodes) != 1 || nodes[0].Statement != "joined report" {
		t.Fatalf("package nodes = %#v, want runtime report after stale save", nodes)
	}
}

func TestStoreAppendNodesDoesNotPartiallyCommit(t *testing.T) {
	store := NewStore()
	if err := store.Save("env", Graph{
		Subgraphs: []Subgraph{{ID: "other"}},
		Nodes:     []Node{{ID: "conflict", Statement: "existing", SubgraphIDs: []string{"other"}}},
	}); err != nil {
		t.Fatal(err)
	}
	err := store.AppendNodes("env", Subgraph{ID: "target"}, []Node{
		{ID: "new", Statement: "must roll back"},
		{ID: "conflict", Statement: "replacement"},
	})
	if err == nil {
		t.Fatal("AppendNodes() error = nil, want conflicting node ID")
	}
	got := store.Load("env")
	if len(got.Nodes) != 1 || got.Nodes[0].Statement != "existing" {
		t.Fatalf("graph after failed AppendNodes = %#v, want original graph", got)
	}
}

func TestStoreDropsExclusiveSubgraphWithoutLeakingMultiOwnedNodes(t *testing.T) {
	t.Parallel()

	store := NewStore()
	private := Subgraph{ID: "private", Kind: SubgraphKindSystem}
	if err := store.AppendNode("task", private, Node{ID: "secret", Statement: "secret"}); err != nil {
		t.Fatal(err)
	}
	graph := store.Load("task").WithSubgraph(Subgraph{ID: "general", Kind: SubgraphKindGeneral})
	graph = graph.WithMemory([]Node{{ID: "public", Statement: "public", SubgraphIDs: []string{"general"}}}, nil)
	graph = graph.WithNodesInSubgraph("general", []string{"secret"})
	graph.Edges = []Edge{{FromRef: "node:secret", ToNodeID: "public", Kind: EdgeKindLogicalAdjacent}}
	store.Save("task", graph)

	if err := store.DropSubgraph("task", "private"); err != nil {
		t.Fatal(err)
	}
	got := store.Load("task")
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "public" || len(got.Edges) != 0 {
		t.Fatalf("graph = %#v, want only public node and no dangling edge", got)
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

func TestStoreRestoreCopiesSnapshotThenIsolatesWrites(t *testing.T) {
	t.Parallel()

	store := NewStore()
	if err := store.SaveSnapshot("ready", Graph{
		Nodes: []Node{{ID: "n1", Statement: "accepted input"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore("target", "ready"); err != nil {
		t.Fatal(err)
	}

	if got := store.Load("target"); len(got.Nodes) != 1 || got.Nodes[0].Statement != "accepted input" {
		t.Fatalf("restored environment = %#v, want accepted input", got)
	}

	if err := store.Save("target", Graph{
		Nodes: []Node{{ID: "n1", Statement: "target update"}},
	}); err != nil {
		t.Fatal(err)
	}
	if store.Load("ready").Nodes[0].Statement != "accepted input" {
		t.Fatal("target write leaked into input snapshot")
	}
	if store.Load("target").Nodes[0].Statement != "target update" {
		t.Fatal("target write did not stay in target")
	}
}

func TestStoreStatsExposeMemoryGraphInventory(t *testing.T) {
	t.Parallel()

	store := NewStore()
	graph := Graph{
		Subgraphs: []Subgraph{{ID: "sg-1"}},
		Nodes:     []Node{{ID: "n-1", Statement: "fact", SubgraphIDs: []string{"sg-1"}}},
		Edges:     []Edge{{FromRef: "subgraph:sg-1", ToNodeID: "n-1", Kind: EdgeKindDerivesFromSubgraph}},
	}
	if err := store.SaveSnapshot("ready", graph); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore("target", "ready"); err != nil {
		t.Fatal(err)
	}

	got := store.Stats()
	if got.Environments != 2 || got.Subgraphs != 2 || got.Nodes != 2 || got.Edges != 2 {
		t.Fatalf("stats = %#v", got)
	}
}
