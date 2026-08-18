package context

import "testing"

func TestStoreForkRecordsBaselineForMerge(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Nodes: []Node{{ID: "mem-1", Statement: "at-fork"}},
	})
	store.Fork("parent", "child")

	store.Save("parent", Graph{
		Nodes: []Node{{ID: "mem-1", Statement: "parent-later"}},
	})
	child := store.Load("child")
	child.Nodes = append(child.Nodes, Node{ID: "c1", Statement: "child-delta"})
	store.Save("child", child)

	store.Fork("parent", "child")
	if got := store.Load("child"); !hasStatement(got, "at-fork") || !hasStatement(got, "child-delta") {
		t.Fatalf("second Fork changed child = %#v", got)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	if !hasStatement(got, "parent-later") {
		t.Fatalf("Merge dropped parent write: %#v", got)
	}
	if hasStatement(got, "at-fork") {
		t.Fatalf("Merge used a later parent snapshot as baseline: %#v", got)
	}
	if !hasStatement(got, "child-delta") {
		t.Fatalf("Merge dropped child delta: %#v", got)
	}
}

func TestStoreMergeKeepsOursAndRenamesCollidingChildNode(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Fork("parent", "child")
	store.Save("parent", Graph{
		Revision:  4,
		Subgraphs: []Subgraph{{ID: "sg-p", Name: "parent"}},
		Nodes: []Node{{
			ID:          "mem-1",
			Statement:   "parent-compact",
			SubgraphIDs: []string{"sg-p"},
		}},
	})
	store.Save("child", Graph{
		Subgraphs: []Subgraph{{ID: "sg-c", Name: "child"}},
		Nodes: []Node{
			{
				ID:          "mem-1",
				Statement:   "child-compact",
				SubgraphIDs: []string{"sg-c"},
			},
			{ID: "n2", Statement: "child-extra"},
		},
		Edges: []Edge{{
			FromRef:  NodeRef("mem-1"),
			ToNodeID: "n2",
			Kind:     EdgeKindLogicalAdjacent,
		}},
	})

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	if got.Revision != 5 {
		t.Fatalf("Revision = %d, want 5", got.Revision)
	}

	ours, ok := nodeByStatement(got, "parent-compact")
	if !ok || ours.ID != "mem-1" {
		t.Fatalf("ours node = %#v, want mem-1 parent-compact", ours)
	}
	theirs, ok := nodeByStatement(got, "child-compact")
	if !ok {
		t.Fatal("child statement missing after Merge")
	}
	if theirs.ID == "mem-1" {
		t.Fatal("child node kept mem-1 and clobbered ours")
	}
	if len(theirs.SubgraphIDs) != 1 || theirs.SubgraphIDs[0] != "sg-c" {
		t.Fatalf("re-ID'd node subgraphs = %#v, want [sg-c]", theirs.SubgraphIDs)
	}
	if !hasStatement(got, "child-extra") {
		t.Fatalf("child-only node missing: %#v", got)
	}
	if !subgraphExists(got, "sg-c") {
		t.Fatalf("child subgraph missing: %#v", got.Subgraphs)
	}

	wantEdge := Edge{FromRef: NodeRef(theirs.ID), ToNodeID: "n2", Kind: EdgeKindLogicalAdjacent}
	if !edgeExists(got, wantEdge) {
		t.Fatalf("rewritten edge missing: edges=%#v want %#v", got.Edges, wantEdge)
	}
	stale := Edge{FromRef: NodeRef("mem-1"), ToNodeID: "n2", Kind: EdgeKindLogicalAdjacent}
	if edgeExists(got, stale) {
		t.Fatal("edge still points at ours mem-1")
	}
}

func TestStoreMergeReplayDoesNotDuplicateNodes(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Fork("parent", "child")
	store.Save("parent", Graph{
		Nodes: []Node{{ID: "mem-1", Statement: "parent-compact"}},
	})
	store.Save("child", Graph{
		Subgraphs: []Subgraph{{ID: "sg-c"}},
		Nodes: []Node{
			{ID: "mem-1", Statement: "child-compact"},
			{ID: "n2", Statement: "child-extra"},
		},
		Edges: []Edge{{
			FromRef:  NodeRef("mem-1"),
			ToNodeID: "n2",
			Kind:     EdgeKindLogicalAdjacent,
		}},
	})

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	first := store.Load("parent")
	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("second Merge: %v", err)
	}
	second := store.Load("parent")
	if len(second.Nodes) != len(first.Nodes) ||
		len(second.Edges) != len(first.Edges) ||
		len(second.Subgraphs) != len(first.Subgraphs) {
		t.Fatalf("replay duplicated graph: first=%#v second=%#v", first, second)
	}
}

func TestStoreMergeUnionsChildOnlyNodeSubgraphAndEdge(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Nodes: []Node{{ID: "p1", Statement: "parent"}},
	})
	store.Fork("parent", "child")

	child := store.Load("child")
	child.Subgraphs = append(child.Subgraphs, Subgraph{ID: "sg-new", Name: "new"})
	child.Nodes = append(child.Nodes, Node{
		ID:          "c1",
		Statement:   "child-only",
		SubgraphIDs: []string{"sg-new"},
	})
	child.Edges = append(child.Edges, Edge{
		FromRef:  NodeRef("p1"),
		ToNodeID: "c1",
		Kind:     EdgeKindLogicalAdjacent,
	})
	store.Save("child", child)

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	if !hasStatement(got, "parent") || !hasStatement(got, "child-only") {
		t.Fatalf("nodes = %#v, want parent and child-only", got.Nodes)
	}
	if !subgraphExists(got, "sg-new") {
		t.Fatalf("subgraphs = %#v, want sg-new", got.Subgraphs)
	}
	want := Edge{FromRef: NodeRef("p1"), ToNodeID: "c1", Kind: EdgeKindLogicalAdjacent}
	if !edgeExists(got, want) {
		t.Fatalf("edges = %#v, want %#v", got.Edges, want)
	}
}

func TestStoreMergeDoesNotRestoreInheritedEdges(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Nodes: []Node{
			{ID: "a", Statement: "a"},
			{ID: "b", Statement: "b"},
		},
		Edges: []Edge{{
			FromRef:  NodeRef("a"),
			ToNodeID: "b",
			Kind:     EdgeKindLogicalAdjacent,
		}},
	})
	store.Fork("parent", "child")
	store.Save("parent", Graph{
		Nodes: []Node{
			{ID: "a", Statement: "a"},
			{ID: "b", Statement: "b"},
		},
	})

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	inherited := Edge{FromRef: NodeRef("a"), ToNodeID: "b", Kind: EdgeKindLogicalAdjacent}
	if edgeExists(got, inherited) {
		t.Fatalf("inherited edge restored after parent deleted it: %#v", got.Edges)
	}
}

func TestStoreMergeReIDsCollisionEvenIfStatementExists(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Fork("parent", "child")
	store.Save("parent", Graph{
		Nodes: []Node{
			{ID: "other", Statement: "child-compact", Kind: NodeKindFact},
			{ID: "mem-1", Statement: "parent-compact", Kind: NodeKindFact},
		},
	})
	store.Save("child", Graph{
		Nodes: []Node{{
			ID:          "mem-1",
			Statement:   "child-compact",
			Kind:        NodeKindHypothesis,
			Status:      NodeStatusDisputed,
			SubgraphIDs: []string{"sg-c"},
		}},
	})

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	var found Node
	n := 0
	for _, node := range got.Nodes {
		if node.Statement == "child-compact" && node.Kind == NodeKindHypothesis {
			found = node
			n++
		}
	}
	if n != 1 {
		t.Fatalf("re-ID'd child nodes = %d in %#v, want 1", n, got.Nodes)
	}
	if found.ID == "mem-1" || found.ID == "other" {
		t.Fatalf("child node id = %q, want a fresh id", found.ID)
	}
	if found.Status != NodeStatusDisputed || len(found.SubgraphIDs) != 1 || found.SubgraphIDs[0] != "sg-c" {
		t.Fatalf("child node lost fields: %#v", found)
	}
}

func TestStoreMergeSkipsIdenticalNode(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("into", Graph{
		Nodes: []Node{{ID: "mem-1", Statement: "same"}},
	})
	store.Save("from", Graph{
		Nodes: []Node{{ID: "mem-1", Statement: "same"}},
	})

	if err := store.Merge("from", "into"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("into")
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "mem-1" || got.Nodes[0].Statement != "same" {
		t.Fatalf("identical node duplicated: %#v", got.Nodes)
	}
}

func TestStoreMergeUnionsIndependentSubgraphs(t *testing.T) {
	t.Parallel()

	store := NewStore()
	store.Save("parent", Graph{
		Subgraphs: []Subgraph{{ID: "sg"}},
		Nodes: []Node{{
			ID:          "mem-1",
			Statement:   "shared",
			SubgraphIDs: []string{"sg"},
		}},
	})
	store.Fork("parent", "child")

	child := store.Load("child")
	child.Subgraphs = append(child.Subgraphs, Subgraph{ID: "sg-b"})
	child.Nodes[0].SubgraphIDs = []string{"sg", "sg-b"}
	store.Save("child", child)

	parent := store.Load("parent")
	parent.Subgraphs = append(parent.Subgraphs, Subgraph{ID: "sg-a"})
	parent.Nodes[0].SubgraphIDs = []string{"sg", "sg-a"}
	store.Save("parent", parent)

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := store.Load("parent")
	node, ok := got.nodeByID("mem-1")
	if !ok {
		t.Fatal("mem-1 missing after Merge")
	}
	if node.Statement != "shared" {
		t.Fatalf("Statement = %q, want shared", node.Statement)
	}
	if !containsID(node.SubgraphIDs, "sg") ||
		!containsID(node.SubgraphIDs, "sg-a") ||
		!containsID(node.SubgraphIDs, "sg-b") {
		t.Fatalf("SubgraphIDs = %#v, want sg ∪ sg-a ∪ sg-b", node.SubgraphIDs)
	}
	if !subgraphExists(got, "sg-b") {
		t.Fatalf("subgraphs = %#v, want sg-b", got.Subgraphs)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("replay Merge: %v", err)
	}
	again, _ := store.Load("parent").nodeByID("mem-1")
	if len(again.SubgraphIDs) != len(node.SubgraphIDs) {
		t.Fatalf("replay duplicated membership: first=%#v second=%#v", node.SubgraphIDs, again.SubgraphIDs)
	}
}

func hasStatement(g Graph, statement string) bool {
	_, ok := nodeByStatement(g, statement)
	return ok
}

func nodeByStatement(g Graph, statement string) (Node, bool) {
	for _, node := range g.Nodes {
		if node.Statement == statement {
			return node, true
		}
	}
	return Node{}, false
}

func subgraphExists(g Graph, id string) bool {
	for _, subgraph := range g.Subgraphs {
		if subgraph.ID == id {
			return true
		}
	}
	return false
}

func edgeExists(g Graph, want Edge) bool {
	for _, edge := range g.Edges {
		if edge == want {
			return true
		}
	}
	return false
}
