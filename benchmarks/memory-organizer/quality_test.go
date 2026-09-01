package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queries.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func intPtr(value int) *int { return &value }

func TestCheckAssertionsReportsEveryUnmetExpectation(t *testing.T) {
	t.Parallel()

	spec := querySpec{
		ID: "prune-and-frontier",
		Assert: assertions{
			MustInclude:        []string{"mem-161"},
			MustExclude:        []string{"mem-999"},
			MinSelected:        intPtr(2),
			MaxSelected:        intPtr(3),
			MustStaySubscribed: []string{"sg-q-3"},
			MustShareWith:      "version-abi",
		},
	}
	selected := []ctxgraph.Node{{ID: "mem-999"}}

	got := checkAssertions(spec, selected, []string{"sg-q-4"}, 0)
	want := []string{
		"must_include mem-161",
		"must_exclude mem-999",
		"expect_min_selected 2 got 1",
		"must_stay_subscribed sg-q-3",
		"must_share_with version-abi",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("failures = %v, want %v", got, want)
	}
	if failures := checkAssertions(querySpec{}, selected, nil, 0); failures != nil {
		t.Fatalf("a query without assertions must not fail: %v", failures)
	}
}

func TestDegradationsFlagsTheColdStartShape(t *testing.T) {
	t.Parallel()

	spec := querySpec{Query: "召回 X"}
	flags := degradations(spec, ctxgraph.Subgraph{Summary: "召回 X"}, nil)
	want := []string{
		"selected=0",
		"missing admission/scope",
		"summary copies the query",
		"subgraph revision=0",
	}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("flags = %v, want %v", flags, want)
	}

	healthy := ctxgraph.Subgraph{
		Summary:   "当前风险前沿",
		Admission: "带证据锚且未闭合",
		Scope:     "ARM 腿未覆盖前有效",
		Revision:  4,
	}
	if flags := degradations(spec, healthy, []ctxgraph.Node{{ID: "mem-1"}}); flags != nil {
		t.Fatalf("healthy case flagged: %v", flags)
	}
}

func call(name, arguments string) agenttool.Call {
	return agenttool.Call{Name: name, Arguments: json.RawMessage(arguments)}
}

func TestMeasureDisciplineCountsMembershipCommittedBelowLevelThree(t *testing.T) {
	t.Parallel()

	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-src"}},
		Nodes: []ctxgraph.Node{
			{ID: "mem-1", SubgraphIDs: []string{"sg-src"}},
			{ID: "mem-2", SubgraphIDs: []string{"sg-src"}},
		},
	}
	exchanges := []exchange{
		{Response: agent.AssistantMessage{
			Usage: &agent.Usage{InputTokens: 12_400},
			ToolCalls: []agenttool.Call{
				call("memory_expand", `{"targets":["sg-src"],"level":2}`),
				call("memory_neighbors", `{"node_ids":["mem-1"]}`),
			},
		}},
		{Response: agent.AssistantMessage{
			Usage: &agent.Usage{InputTokens: 30_000},
			ToolCalls: []agenttool.Call{
				call("memory_expand", `{"targets":["mem-1"],"level":3}`),
				call("memory_add_to_subgraph", `{"subgraph_id":"sg-q-3","node_ids":["mem-1","mem-2"]}`),
				call("memory_apply", `{"ops":[{"action":"attach","id":"mem-2","reason":"r"}]}`),
				call("memory_collapse", `{}`),
			},
		}},
	}

	got := measureDiscipline(graph, exchanges)
	if got.MembershipCommits != 3 || got.MembershipCommitsWithoutL3 != 2 {
		t.Fatalf("membership = %d commits, %d below level 3; want 3 and 2 (only mem-1 was read)", got.MembershipCommits, got.MembershipCommitsWithoutL3)
	}
	if got.NavigationCalls != 1 || got.ExpandCalls != 2 || got.CollapseCalls != 1 {
		t.Fatalf("tool usage = %#v", got)
	}
	if got.PeakInputTokens != 30_000 {
		t.Fatalf("peak input = %d, want the largest single request", got.PeakInputTokens)
	}
}

func TestMeasureDisciplineCountsSessionResetsFromShrinkingHistory(t *testing.T) {
	t.Parallel()

	exchanges := []exchange{
		{Request: agent.Request{Messages: make([]agent.Message, 4)}},
		{Request: agent.Request{Messages: make([]agent.Message, 9)}},
		{Request: agent.Request{Messages: make([]agent.Message, 2)}},
		{Request: agent.Request{Messages: make([]agent.Message, 3)}},
	}
	if got := measureDiscipline(ctxgraph.Graph{}, exchanges).SessionResets; got != 1 {
		t.Fatalf("session resets = %d, want 1", got)
	}
}

func TestReadQuerySpecsRejectsAForwardSharingAssertion(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, `[
	  {"id":"a","query":"q","assert":{"must_share_with":"b"}},
	  {"id":"b","query":"q"}
	]`)
	if _, err := readQuerySpecs(path); err == nil {
		t.Fatal("must_share_with pointing at a later query should be rejected")
	}
}

func TestReadQuerySpecsRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, `[{"id":"a","query":"q","mode":"rewrite"}]`)
	if _, err := readQuerySpecs(path); err == nil {
		t.Fatal("unknown mode should be rejected")
	}
}

func TestSlimKeepsMetricsAndDropsTheTrace(t *testing.T) {
	t.Parallel()

	result := caseResult{
		CaseFile:  "prune-and-frontier.json",
		Selected:  []ctxgraph.Node{{ID: "mem-1"}},
		Exchanges: []exchange{{}},
		Events:    []event.RuntimeEvent{{}},
	}
	got := slim(result)
	if got.Exchanges != nil || got.Events != nil {
		t.Fatalf("slim kept the trace: %#v", got)
	}
	if got.CaseFile == "" || len(got.Selected) != 1 {
		t.Fatalf("slim dropped the parts the summary needs: %#v", got)
	}
}
