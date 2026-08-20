package context

import (
	"path/filepath"
	"testing"
)

func TestOpenStoreRestoresGraphsAndForkBaselines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	first, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first.Save("parent", Graph{Nodes: []Node{{ID: "base", Statement: "base"}}})
	first.Fork("parent", "child")
	first.Save("child", Graph{Nodes: []Node{
		{ID: "base", Statement: "base"},
		{ID: "from-child", Statement: "child"},
	}})
	first.Save("parent", Graph{Nodes: []Node{
		{ID: "base", Statement: "base"},
		{ID: "from-parent", Statement: "parent"},
	}})
	second, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Merge("child", "parent"); err != nil {
		t.Fatal(err)
	}
	got := second.Load("parent")
	for _, id := range []string{"base", "from-parent", "from-child"} {
		if _, ok := got.nodeByID(id); !ok {
			t.Fatalf("restored merge missing %q: %#v", id, got.Nodes)
		}
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

func TestStoreForkFailureDoesNotCreateChild(t *testing.T) {
	store := NewStore()
	if err := store.Save("parent", Graph{Nodes: []Node{{ID: "parent", Statement: "parent"}}}); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(t.TempDir(), "missing", "memory.json")

	err := store.Fork("parent", "child")
	if err == nil {
		t.Fatal("Fork() error = nil, want persistence error")
	}
	if got := store.Load("child"); len(got.Nodes) != 0 {
		t.Fatalf("Load(child) after failed Fork = %#v, want empty graph", got.Nodes)
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

func TestStoreStatsExposeMemoryGraphInventory(t *testing.T) {
	t.Parallel()

	store := NewStore()
	graph := Graph{
		Subgraphs: []Subgraph{{ID: "sg-1"}},
		Nodes:     []Node{{ID: "n-1", Statement: "fact", SubgraphIDs: []string{"sg-1"}}},
		Edges:     []Edge{{FromRef: "subgraph:sg-1", ToNodeID: "n-1", Kind: EdgeKindDerivesFromSubgraph}},
	}
	if err := store.Save("parent", graph); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}

	got := store.Stats()
	if got.Environments != 2 || got.Baselines != 1 || got.Subgraphs != 2 || got.Nodes != 2 || got.Edges != 2 {
		t.Fatalf("stats = %#v", got)
	}
}
