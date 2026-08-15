package context

import (
	"reflect"
	"testing"
)

func TestGraphNodesInSubgraphs(t *testing.T) {
	t.Parallel()

	shared := Node{
		ID:          "shared",
		Kind:        NodeKindFact,
		Statement:   "shared fact",
		Status:      NodeStatusAccepted,
		SubgraphIDs: []string{"sg-a", "sg-b"},
	}
	onlyA := Node{
		ID:          "only-a",
		Kind:        NodeKindDirective,
		Statement:   "only in a",
		Status:      NodeStatusAccepted,
		SubgraphIDs: []string{"sg-a"},
	}
	onlyC := Node{
		ID:          "only-c",
		Kind:        NodeKindHypothesis,
		Statement:   "only in c",
		Status:      NodeStatusAccepted,
		SubgraphIDs: []string{"sg-c"},
	}
	graph := Graph{
		Nodes: []Node{shared, onlyA, onlyC, {
			ID:          "shared",
			Statement:   "duplicate id ignored",
			SubgraphIDs: []string{"sg-a"},
		}},
	}

	tests := []struct {
		name     string
		ids      []string
		expected []Node
	}{
		{
			name:     "empty subscriptions",
			ids:      []string{},
			expected: []Node{},
		},
		{
			name:     "unknown subgraph",
			ids:      []string{"sg-missing"},
			expected: []Node{},
		},
		{
			name:     "single subgraph",
			ids:      []string{"sg-c"},
			expected: []Node{onlyC},
		},
		{
			name:     "union keeps graph order and drops duplicates",
			ids:      []string{"sg-b", "sg-a", "sg-a", ""},
			expected: []Node{shared, onlyA},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := graph.NodesInSubgraphs(tt.ids)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("NodesInSubgraphs() = %#v, want %#v", got, tt.expected)
			}
		})
	}
}

func TestGraphSubgraphsOf(t *testing.T) {
	t.Parallel()

	graph := Graph{
		Subgraphs: []Subgraph{{ID: "sg-a"}, {ID: "sg-b"}},
		Nodes: []Node{
			{
				ID:          "n1",
				SubgraphIDs: []string{"sg-b", "sg-a", "sg-b", "sg-missing"},
			},
			{
				ID:          "n1",
				SubgraphIDs: []string{"sg-a"},
			},
			{
				ID: "n2",
			},
		},
	}

	tests := []struct {
		name     string
		nodeID   string
		expected []string
	}{
		{
			name:     "unknown node",
			nodeID:   "missing",
			expected: []string{},
		},
		{
			name:     "empty node id",
			nodeID:   "",
			expected: []string{},
		},
		{
			name:     "first node wins and membership is deduped",
			nodeID:   "n1",
			expected: []string{"sg-b", "sg-a", "sg-missing"},
		},
		{
			name:     "node with no subgraphs",
			nodeID:   "n2",
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := graph.SubgraphsOf(tt.nodeID)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("SubgraphsOf() = %#v, want %#v", got, tt.expected)
			}
		})
	}
}

func TestGraphSourceSubgraphsOf(t *testing.T) {
	t.Parallel()

	graph := Graph{
		Nodes: []Node{{ID: "n1"}, {ID: "n2"}},
		Edges: []Edge{
			{FromRef: "subgraph:sg-a", ToNodeID: "n1", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "node:n0", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "subgraph:sg-b", ToNodeID: "n1", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "subgraph:sg-a", ToNodeID: "n1", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "subgraph:sg-c", ToNodeID: "n2", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "subgraph:", ToNodeID: "n1", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "subgraph:sg-d", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "subgraph:sg-ghost", ToNodeID: "ghost", Kind: EdgeKindDerivesFromSubgraph},
		},
	}

	tests := []struct {
		name     string
		nodeID   string
		expected []string
	}{
		{
			name:     "unknown node",
			nodeID:   "missing",
			expected: []string{},
		},
		{
			name:     "missing node ignores dangling source edges",
			nodeID:   "ghost",
			expected: []string{},
		},
		{
			name:     "empty node id",
			nodeID:   "",
			expected: []string{},
		},
		{
			name:     "derives from subgraph edges in graph order",
			nodeID:   "n1",
			expected: []string{"sg-a", "sg-b"},
		},
		{
			name:     "other node sources stay isolated",
			nodeID:   "n2",
			expected: []string{"sg-c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := graph.SourceSubgraphsOf(tt.nodeID)
			if !reflect.DeepEqual(got, tt.expected) {
				t.Fatalf("SourceSubgraphsOf() = %#v, want %#v", got, tt.expected)
			}
		})
	}
}

func TestGraphUpstreamAndDownstreamNodes(t *testing.T) {
	t.Parallel()

	n0 := Node{ID: "n0", Statement: "zero", Status: NodeStatusAccepted}
	n1 := Node{ID: "n1", Statement: "one", Status: NodeStatusAccepted}
	n2 := Node{ID: "n2", Statement: "two", Status: NodeStatusAccepted}
	n3 := Node{ID: "n3", Statement: "three", Status: NodeStatusAccepted}
	graph := Graph{
		Nodes: []Node{n0, n1, n2, n3},
		Edges: []Edge{
			{FromRef: "node:n0", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "subgraph:sg-a", ToNodeID: "n1", Kind: EdgeKindDerivesFromSubgraph},
			{FromRef: "node:n2", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "node:n1", ToNodeID: "n3", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "node:n1", ToNodeID: "n3", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "node:", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "node:missing", ToNodeID: "n1", Kind: EdgeKindLogicalAdjacent},
			{FromRef: "node:n1", ToNodeID: "missing", Kind: EdgeKindLogicalAdjacent},
		},
	}

	tests := []struct {
		name               string
		nodeID             string
		expectedUpstream   []Node
		expectedDownstream []Node
	}{
		{
			name:               "unknown node",
			nodeID:             "missing",
			expectedUpstream:   []Node{},
			expectedDownstream: []Node{},
		},
		{
			name:               "empty node id",
			nodeID:             "",
			expectedUpstream:   []Node{},
			expectedDownstream: []Node{},
		},
		{
			name:               "neighbors follow node edges and skip missing refs",
			nodeID:             "n1",
			expectedUpstream:   []Node{n0, n2},
			expectedDownstream: []Node{n3},
		},
		{
			name:               "leaf has upstream only",
			nodeID:             "n3",
			expectedUpstream:   []Node{n1},
			expectedDownstream: []Node{},
		},
		{
			name:               "root has downstream only",
			nodeID:             "n0",
			expectedUpstream:   []Node{},
			expectedDownstream: []Node{n1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotUp := graph.UpstreamNodes(tt.nodeID)
			if !reflect.DeepEqual(gotUp, tt.expectedUpstream) {
				t.Fatalf("UpstreamNodes() = %#v, want %#v", gotUp, tt.expectedUpstream)
			}
			gotDown := graph.DownstreamNodes(tt.nodeID)
			if !reflect.DeepEqual(gotDown, tt.expectedDownstream) {
				t.Fatalf("DownstreamNodes() = %#v, want %#v", gotDown, tt.expectedDownstream)
			}
		})
	}
}

func TestGraphWithMemoryAppendsNodesAndEdges(t *testing.T) {
	t.Parallel()

	original := Graph{
		Revision: 3,
		Nodes:    []Node{{ID: "old", Statement: "prior", SubgraphIDs: []string{"sg-a"}}},
	}
	got := original.WithMemory(
		[]Node{{ID: "new", Statement: "added", SubgraphIDs: []string{"sg-a"}}},
		[]Edge{{FromRef: "node:old", ToNodeID: "new", Kind: EdgeKindLogicalAdjacent}},
	)
	if got.Revision != 4 {
		t.Fatalf("revision = %d, want 4", got.Revision)
	}
	if len(original.Nodes) != 1 {
		t.Fatal("WithMemory mutated the original graph")
	}

	nodes := got.NodesInSubgraphs([]string{"sg-a"})
	if len(nodes) != 2 || nodes[1].ID != "new" {
		t.Fatalf("nodes = %#v", nodes)
	}
	upstream := got.UpstreamNodes("new")
	if len(upstream) != 1 || upstream[0].ID != "old" {
		t.Fatalf("upstream = %#v", upstream)
	}
}

func TestGraphWithNodesInSubgraph(t *testing.T) {
	t.Parallel()

	original := Graph{
		Revision: 2,
		Subgraphs: []Subgraph{{
			ID:       "sg-b",
			Revision: 4,
		}},
		Nodes: []Node{
			{ID: "n0", SubgraphIDs: []string{"sg-a"}},
			{ID: "n1", SubgraphIDs: []string{"sg-b"}},
			{ID: "n0", SubgraphIDs: []string{"sg-a"}},
		},
	}

	got := original.WithNodesInSubgraph("sg-b", []string{"n0", "n1", "missing", "n0", ""})
	if got.Revision != 3 {
		t.Fatalf("revision = %d, want 3", got.Revision)
	}
	if original.Nodes[0].SubgraphIDs[0] != "sg-a" || len(original.Nodes[0].SubgraphIDs) != 1 {
		t.Fatal("WithNodesInSubgraph mutated the original graph")
	}
	if !reflect.DeepEqual(got.Nodes[0].SubgraphIDs, []string{"sg-a", "sg-b"}) {
		t.Fatalf("n0 subgraphs = %v, want [sg-a sg-b]", got.Nodes[0].SubgraphIDs)
	}
	if !reflect.DeepEqual(got.Nodes[1].SubgraphIDs, []string{"sg-b"}) {
		t.Fatalf("n1 subgraphs = %v, want [sg-b]", got.Nodes[1].SubgraphIDs)
	}
	if got.Subgraphs[0].Revision != 5 {
		t.Fatalf("subgraph revision = %d, want 5", got.Subgraphs[0].Revision)
	}

	unchanged := original.WithNodesInSubgraph("sg-b", []string{"n1"})
	if unchanged.Revision != 2 {
		t.Fatalf("unchanged revision = %d, want 2", unchanged.Revision)
	}
}

func TestGraphWithSubgraph(t *testing.T) {
	t.Parallel()

	original := Graph{
		Revision:  1,
		Subgraphs: []Subgraph{{ID: "sg-a", Name: "old", Revision: 2}},
	}
	got := original.WithSubgraph(Subgraph{
		ID:      "sg-b",
		Name:    "query",
		Summary: "blue",
		Kind:    SubgraphKindTask,
	})
	if got.Revision != 2 {
		t.Fatalf("revision = %d, want 2", got.Revision)
	}
	if len(original.Subgraphs) != 1 {
		t.Fatal("WithSubgraph mutated the original graph")
	}
	if len(got.Subgraphs) != 2 || got.Subgraphs[1].ID != "sg-b" {
		t.Fatalf("subgraphs = %#v", got.Subgraphs)
	}

	replaced := got.WithSubgraph(Subgraph{ID: "sg-a", Name: "new", Revision: 3})
	if replaced.Subgraphs[0].Name != "new" || replaced.Revision != 3 {
		t.Fatalf("replace = %#v", replaced.Subgraphs)
	}
}
