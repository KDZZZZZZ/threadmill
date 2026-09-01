package main

import (
	"path/filepath"
	"reflect"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestNewEvalStoreKeepsSourceReadOnlyAndPersistsAcrossQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	source, err := ctxgraph.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "history", Kind: ctxgraph.SubgraphKindGeneral}},
		Nodes: []ctxgraph.Node{{
			ID:          "fact-1",
			Kind:        ctxgraph.NodeKindFact,
			Status:      ctxgraph.NodeStatusAccepted,
			Statement:   "original fact",
			SubgraphIDs: []string{"history"},
		}},
	}
	if err := source.Save("env-30", want); err != nil {
		t.Fatal(err)
	}
	before, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}

	eval, err := newEvalStore(path, "env-30")
	if err != nil {
		t.Fatal(err)
	}
	mutated := eval.Load(evalEnvID)
	mutated.Nodes[0].Statement = "mutated by first query"
	if err := eval.Save(evalEnvID, mutated); err != nil {
		t.Fatal(err)
	}
	if got := eval.Load(evalEnvID).Nodes[0].Statement; got != "mutated by first query" {
		t.Fatalf("second query starts with %q, want first query result", got)
	}
	after, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("source memory changed: before %s, after %s", before, after)
	}
}

func TestUpdateSubscriptionsPersistsAcrossQueries(t *testing.T) {
	t.Parallel()

	first := updateSubscriptions(
		[]string{"sg-q-1", "sg-q-2"},
		nil,
		[]string{"sg-q-1", "sg-q-2"},
		"sg-q-3",
	)
	if want := []string{"sg-q-3"}; !reflect.DeepEqual(first, want) {
		t.Fatalf("first subscriptions = %v, want %v", first, want)
	}
	second := updateSubscriptions(first, []string{"shared"}, nil, "sg-q-4")
	if want := []string{"sg-q-3", "shared", "sg-q-4"}; !reflect.DeepEqual(second, want) {
		t.Fatalf("second subscriptions = %v, want %v", second, want)
	}
}

func TestDiffGraphIncludesIncidentalOrganizerChanges(t *testing.T) {
	t.Parallel()

	before := ctxgraph.Graph{
		Revision:  1,
		Subgraphs: []ctxgraph.Subgraph{{ID: "old", Summary: "broad"}},
		Nodes: []ctxgraph.Node{
			{ID: "changed", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"old"}},
			{ID: "deleted", Status: ctxgraph.NodeStatusOutdated},
		},
	}
	after := ctxgraph.Graph{
		Revision: 2,
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "old", Summary: "narrow"},
			{ID: "target", Summary: "query result"},
		},
		Nodes: []ctxgraph.Node{
			{ID: "changed", Status: ctxgraph.NodeStatusDisputed, SubgraphIDs: []string{"old", "target"}},
			{ID: "added", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"target"}},
		},
	}

	delta := diffGraph(before, after)
	if len(delta.NodesAdded) != 1 || delta.NodesAdded[0].ID != "added" {
		t.Fatalf("added nodes = %#v", delta.NodesAdded)
	}
	if len(delta.NodesDeleted) != 1 || delta.NodesDeleted[0].ID != "deleted" {
		t.Fatalf("deleted nodes = %#v", delta.NodesDeleted)
	}
	if len(delta.NodesChanged) != 1 || delta.NodesChanged[0].After.ID != "changed" {
		t.Fatalf("changed nodes = %#v", delta.NodesChanged)
	}
	if len(delta.SubgraphsAdded) != 1 || delta.SubgraphsAdded[0].ID != "target" {
		t.Fatalf("added subgraphs = %#v", delta.SubgraphsAdded)
	}
	if len(delta.SubgraphsChanged) != 1 || delta.SubgraphsChanged[0].After.Summary != "narrow" {
		t.Fatalf("changed subgraphs = %#v", delta.SubgraphsChanged)
	}
}

// The cold-start comparison joins on query ID, so the control group is a
// contract: extended scenarios may be added, never inserted into that list.
func TestShippedQueriesKeepTheControlGroupIntact(t *testing.T) {
	t.Parallel()

	specs, err := readQuerySpecs("queries.json")
	if err != nil {
		t.Fatal(err)
	}
	var control []string
	extendedStarted := false
	for _, spec := range specs {
		if spec.Group == "extended" {
			extendedStarted = true
			continue
		}
		if extendedStarted {
			t.Fatalf("control query %q comes after the extended group", spec.ID)
		}
		control = append(control, spec.ID)
	}
	want := []string{
		"prune-and-frontier",
		"legacy-cleanup",
		"probe-reconfiguration",
		"install-consumers",
		"ctest-matrix",
		"version-abi",
	}
	if !reflect.DeepEqual(control, want) {
		t.Fatalf("control queries = %v, want %v", control, want)
	}
	scenarios := map[string]bool{}
	for _, spec := range specs {
		scenarios[spec.ID] = true
	}
	for _, id := range []string{
		"absent-topic-negative-control",
		"partial-invalidation",
		"multi-membership",
		"deep-curation",
	} {
		if !scenarios[id] {
			t.Fatalf("extended scenario %q is missing", id)
		}
	}
}
