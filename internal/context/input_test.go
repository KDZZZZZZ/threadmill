package context_test

import (
	"reflect"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestPartitionInputsSingleSourceIsCommonAndIsolated(t *testing.T) {
	t.Parallel()
	graph := ctxgraph.Graph{
		Revision:  12,
		Subgraphs: []ctxgraph.Subgraph{{ID: "knowledge", Revision: 5}},
		Nodes: []ctxgraph.Node{{
			ID: "n", Statement: "a fact", SubgraphIDs: []string{"knowledge"},
			SourceRefs: []string{"source-file"},
		}},
		Edges: []ctxgraph.Edge{{FromRef: "subgraph:knowledge", ToNodeID: "n", Kind: ctxgraph.EdgeKindDerivesFromSubgraph}},
	}
	input := ctxgraph.PartitionInputs([]ctxgraph.InputSource{{Ref: "output-a", Graph: graph}})
	if input.HasDifferences() {
		t.Fatal("a single source must not require organization")
	}
	if !reflect.DeepEqual(input.Common, graph) {
		t.Fatalf("common = %#v, want source snapshot %#v", input.Common, graph)
	}
	input.Common.Nodes[0].SourceRefs[0] = "changed"
	if graph.Nodes[0].SourceRefs[0] != "source-file" {
		t.Fatal("common memory shares writable data with its source")
	}
}

func TestPartitionInputsUsesAllSourcesAndSemanticValues(t *testing.T) {
	t.Parallel()
	common := ctxgraph.Node{
		ID: "common", Statement: "required behavior", Kind: ctxgraph.NodeKindDirective,
		SubgraphIDs: []string{"general", "task"}, SourceRefs: []string{"user", "brief"},
	}
	first := ctxgraph.Graph{
		Revision: 2,
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "general", Revision: 1}, {ID: "task", Scope: "before validation"},
		},
		Nodes: []ctxgraph.Node{
			common,
			{ID: "state", Statement: "feature exists", Status: ctxgraph.NodeStatusAccepted},
			{ID: "old", Statement: "old knowledge only in two sources"},
		},
		Edges: []ctxgraph.Edge{{FromRef: "node:common", ToNodeID: "state", Kind: ctxgraph.EdgeKindLogicalAdjacent}},
	}
	second := first.Clone()
	second.Revision = 40
	second.Subgraphs[0].Revision = 19
	second.Subgraphs[1].Scope = "after validation"
	second.Nodes[0].SubgraphIDs = []string{"task", "general"}
	second.Nodes[0].SourceRefs = []string{"brief", "user"}
	second.Nodes[1].Status = ctxgraph.NodeStatusDisputed
	third := first.Clone()
	third.Nodes = third.Nodes[:2]

	input := ctxgraph.PartitionInputs([]ctxgraph.InputSource{
		{Ref: "a", Graph: first}, {Ref: "b", Graph: second}, {Ref: "c", Graph: third},
	})
	if !input.HasDifferences() {
		t.Fatal("status changes, scope changes and values missing from one source require processing")
	}
	if len(input.Common.Nodes) != 1 || !reflect.DeepEqual(input.Common.Nodes[0], common) {
		t.Fatalf("common nodes = %#v; only common should bypass organization", input.Common.Nodes)
	}
	if len(input.Common.Subgraphs) != 1 || input.Common.Subgraphs[0].ID != "general" {
		t.Fatalf("common subgraphs = %#v; revision alone is not a difference", input.Common.Subgraphs)
	}
	if !reflect.DeepEqual(input.Common.Edges, first.Edges) {
		t.Fatalf("common relationship must survive even when its endpoint differs: %#v", input.Common.Edges)
	}
	for i, want := range [][]string{{"state", "old"}, {"state", "old"}, {"state"}} {
		if input.Differences[i].Ref != []string{"a", "b", "c"}[i] {
			t.Fatal("source provenance/order was lost")
		}
		var got []string
		for _, node := range input.Differences[i].Graph.Nodes {
			got = append(got, node.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("source %d differences = %v, want %v", i, got, want)
		}
	}
}

func TestInputDraftPreservesConflictingVersionsAndTheirReferences(t *testing.T) {
	t.Parallel()
	first := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "knowledge", Scope: "implementation"}},
		Nodes: []ctxgraph.Node{
			{ID: "shared", Statement: "shared requirement", Kind: ctxgraph.NodeKindDirective},
			{ID: "result", Statement: "enabled", Status: ctxgraph.NodeStatusAccepted,
				SubgraphIDs: []string{"knowledge"}, SourceRefs: []string{"test-output"}},
		},
		Edges: []ctxgraph.Edge{{FromRef: "node:shared", ToNodeID: "result", Kind: ctxgraph.EdgeKindLogicalAdjacent}},
	}
	second := first.Clone()
	second.Nodes[1].Statement = "disabled"
	second.Subgraphs[0].Scope = "validation"
	input := ctxgraph.PartitionInputs([]ctxgraph.InputSource{{Ref: "a", Graph: first}, {Ref: "b", Graph: second}})
	draft, err := input.Draft()
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Nodes) != 3 || len(draft.Subgraphs) != 2 || len(draft.Edges) != 2 {
		t.Fatalf("draft lost a source variant or relationship: %#v", draft)
	}
	var alternative ctxgraph.Node
	for _, node := range draft.Nodes {
		if node.Statement == "disabled" {
			alternative = node
		}
	}
	if alternative.ID == "" || alternative.ID == "result" || alternative.SubgraphIDs[0] == "knowledge" {
		t.Fatalf("alternative was not remapped to its own node/subgraph: %#v", alternative)
	}
	if !reflect.DeepEqual(alternative.SourceRefs, []string{"test-output", "b"}) {
		t.Fatalf("alternative provenance = %v", alternative.SourceRefs)
	}
	if len(draft.UpstreamNodes(alternative.ID)) != 1 || draft.UpstreamNodes(alternative.ID)[0].ID != "shared" {
		t.Fatal("common relationship no longer reaches its source-specific endpoint")
	}
	composed, err := input.Compose(draft)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(composed.Nodes[0], first.Nodes[0]) {
		t.Fatal("common memory changed while composing differences")
	}
}

func TestInputComposeProtectsDirectivesAndRequiresAcceptedCorrections(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		kind             string
		correctionStatus string
	}{
		{name: "common directive", kind: ctxgraph.NodeKindDirective, correctionStatus: ctxgraph.NodeStatusAccepted},
		{name: "unverified correction", kind: ctxgraph.NodeKindFact, correctionStatus: ctxgraph.NodeStatusDisputed},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := ctxgraph.Node{ID: "common", Kind: test.kind, Statement: "original", Status: ctxgraph.NodeStatusAccepted}
			input := ctxgraph.InputPartition{Common: ctxgraph.Graph{Nodes: []ctxgraph.Node{original}}}
			changed := original
			changed.Status = ctxgraph.NodeStatusSuperseded
			changed.SupersededBy = "correction"
			_, err := input.Compose(ctxgraph.Graph{Nodes: []ctxgraph.Node{
				changed, {ID: "correction", Kind: ctxgraph.NodeKindFact, Statement: "replacement", Status: test.correctionStatus},
			}})
			if err == nil {
				t.Fatal("common memory was invalidated without a valid accepted correction")
			}
		})
	}
}

func TestInputComposeRejectsDuplicateIdentitiesAndDanglingCommonEdges(t *testing.T) {
	t.Parallel()
	common := ctxgraph.Node{ID: "common", Statement: "original"}
	input := ctxgraph.InputPartition{Common: ctxgraph.Graph{
		Nodes: []ctxgraph.Node{common},
		Edges: []ctxgraph.Edge{{FromRef: "node:common", ToNodeID: "different", Kind: ctxgraph.EdgeKindLogicalAdjacent}},
	}}
	for _, test := range []struct {
		name  string
		nodes []ctxgraph.Node
	}{
		{name: "duplicate common identity", nodes: []ctxgraph.Node{common, common, {ID: "different", Statement: "candidate"}}},
		{name: "dangling common edge"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := input.Compose(ctxgraph.Graph{Nodes: test.nodes}); err == nil {
				t.Fatal("invalid composed graph was accepted")
			}
		})
	}
}
