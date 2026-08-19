package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestMemoryToolsDefinitions(t *testing.T) {
	tools := MemoryTools(func() ctxgraph.Copy { return ctxgraph.Copy{} }, nil)
	if len(tools) != 5 {
		t.Fatalf("MemoryTools() len = %d, want 5", len(tools))
	}

	want := []string{
		"memory_neighbors",
		"memory_subgraphs_of",
		"memory_sources_of",
		"memory_nodes_in",
		"memory_add_to_subgraph",
	}
	for i, tool := range tools {
		def := tool.Definition()
		if def.Name != want[i] {
			t.Fatalf("tool %d name = %q, want %q", i, def.Name, want[i])
		}
		if err := def.Validate(); err != nil {
			t.Fatalf("%s.Validate() = %v", def.Name, err)
		}
	}
}

func TestMemoryNeighborsSignedWindow(t *testing.T) {
	copy := seedMemoryCopy()

	tests := []struct {
		name         string
		arguments    string
		wantBefore   []string
		wantAfter    []string
		wantErr      bool
		errSubstring string
	}{
		{
			name:       "omitted windows return full reachable chains in graph order",
			arguments:  `{"node_id":"n2"}`,
			wantBefore: []string{"n0", "n1"},
			wantAfter:  []string{"n3", "n4"},
		},
		{
			name:       "negative before takes from the front of upstream",
			arguments:  `{"node_id":"n2","before":-1}`,
			wantBefore: []string{"n0"},
			wantAfter:  []string{"n3", "n4"},
		},
		{
			name:       "positive before takes from the end of upstream",
			arguments:  `{"node_id":"n2","before":1}`,
			wantBefore: []string{"n1"},
			wantAfter:  []string{"n3", "n4"},
		},
		{
			name:       "negative after takes from the front of downstream",
			arguments:  `{"node_id":"n2","after":-1}`,
			wantBefore: []string{"n0", "n1"},
			wantAfter:  []string{"n3"},
		},
		{
			name:       "positive after takes from the end of downstream",
			arguments:  `{"node_id":"n2","after":1}`,
			wantBefore: []string{"n0", "n1"},
			wantAfter:  []string{"n4"},
		},
		{
			name:       "zero windows are empty",
			arguments:  `{"node_id":"n2","before":0,"after":0}`,
			wantBefore: []string{},
			wantAfter:  []string{},
		},
		{
			name:       "unknown node returns empty lists",
			arguments:  `{"node_id":"missing"}`,
			wantBefore: []string{},
			wantAfter:  []string{},
		},
		{
			name:         "missing node_id is an error",
			arguments:    `{}`,
			wantErr:      true,
			errSubstring: "node_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := executeMemory(t, &copy, "memory_neighbors", tt.arguments)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Execute() error = nil, want error")
				}
				if !strings.Contains(err.Error(), tt.errSubstring) {
					t.Fatalf("Execute() error = %v, want substring %q", err, tt.errSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var got struct {
				Before []memoryNode `json:"before"`
				After  []memoryNode `json:"after"`
			}
			if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
				t.Fatalf("decode output %q: %v", out.Content, err)
			}
			if !reflect.DeepEqual(nodeIDs(got.Before), tt.wantBefore) {
				t.Fatalf("before = %v, want %v", nodeIDs(got.Before), tt.wantBefore)
			}
			if !reflect.DeepEqual(nodeIDs(got.After), tt.wantAfter) {
				t.Fatalf("after = %v, want %v", nodeIDs(got.After), tt.wantAfter)
			}
		})
	}
}

func TestMemorySubgraphsOf(t *testing.T) {
	copy := seedMemoryCopy()

	tests := []struct {
		name      string
		arguments string
		want      []string
		wantErr   bool
	}{
		{
			name:      "returns membership subgraphs",
			arguments: `{"node_id":"n2"}`,
			want:      []string{"sg-a", "sg-b"},
		},
		{
			name:      "unknown node is empty",
			arguments: `{"node_id":"missing"}`,
			want:      []string{},
		},
		{
			name:      "missing node_id is an error",
			arguments: `{}`,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := executeMemory(t, &copy, "memory_subgraphs_of", tt.arguments)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Execute() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var got struct {
				SubgraphIDs []string `json:"subgraph_ids"`
			}
			if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
				t.Fatalf("decode output %q: %v", out.Content, err)
			}
			if !reflect.DeepEqual(got.SubgraphIDs, tt.want) {
				t.Fatalf("subgraph_ids = %v, want %v", got.SubgraphIDs, tt.want)
			}
		})
	}
}

func TestMemorySourcesOf(t *testing.T) {
	copy := seedMemoryCopy()

	out, err := executeMemory(t, &copy, "memory_sources_of", `{"node_id":"n2"}`)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		SourceSubgraphIDs []string `json:"source_subgraph_ids"`
	}
	if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.Content, err)
	}
	want := []string{"sg-src", "sg-a"}
	if !reflect.DeepEqual(got.SourceSubgraphIDs, want) {
		t.Fatalf("source_subgraph_ids = %v, want %v", got.SourceSubgraphIDs, want)
	}
}

func TestMemoryNodesIn(t *testing.T) {
	copy := seedMemoryCopy()

	tests := []struct {
		name      string
		arguments string
		want      []string
	}{
		{
			name:      "union keeps graph order",
			arguments: `{"subgraph_ids":["sg-b","sg-a"]}`,
			want:      []string{"n0", "n1", "n2", "n3", "n4"},
		},
		{
			name:      "single subgraph",
			arguments: `{"subgraph_ids":["sg-b"]}`,
			want:      []string{"n2", "n3", "n4"},
		},
		{
			name:      "empty subscriptions",
			arguments: `{"subgraph_ids":[]}`,
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := executeMemory(t, &copy, "memory_nodes_in", tt.arguments)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}

			var got struct {
				Nodes []memoryNode `json:"nodes"`
			}
			if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
				t.Fatalf("decode output %q: %v", out.Content, err)
			}
			if !reflect.DeepEqual(nodeIDs(got.Nodes), tt.want) {
				t.Fatalf("nodes = %v, want %v", nodeIDs(got.Nodes), tt.want)
			}
		})
	}
}

func TestMemoryAddToSubgraph(t *testing.T) {
	copy := seedMemoryCopy()

	out, err := executeMemory(t, &copy, "memory_add_to_subgraph", `{"subgraph_id":"sg-b","node_ids":["n0","missing","n0"]}`)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		SubgraphID string   `json:"subgraph_id"`
		Added      []string `json:"added"`
		Missing    []string `json:"missing"`
	}
	if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.Content, err)
	}
	if got.SubgraphID != "sg-b" {
		t.Fatalf("subgraph_id = %q, want sg-b", got.SubgraphID)
	}
	if !reflect.DeepEqual(got.Added, []string{"n0"}) {
		t.Fatalf("added = %v, want [n0]", got.Added)
	}
	if !reflect.DeepEqual(got.Missing, []string{"missing"}) {
		t.Fatalf("missing = %v, want [missing]", got.Missing)
	}

	ids := nodeIDs(compactMemoryNodes(copy.Graph.NodesInSubgraphs([]string{"sg-b"})))
	if !reflect.DeepEqual(ids, []string{"n0", "n2", "n3", "n4"}) {
		t.Fatalf("sg-b nodes = %v, want [n0 n2 n3 n4]", ids)
	}
}

func TestMemoryAddToSubgraphRequiresCommit(t *testing.T) {
	copy := seedMemoryCopy()
	var tool Tool
	for _, candidate := range MemoryTools(func() ctxgraph.Copy { return copy }, nil) {
		if candidate.Definition().Name == "memory_add_to_subgraph" {
			tool = candidate
			break
		}
	}
	_, err := tool.Execute(context.Background(), Call{
		ID:        "call-1",
		Name:      "memory_add_to_subgraph",
		Arguments: json.RawMessage(`{"subgraph_id":"sg-b","node_ids":["n0"]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("Execute() error = %v, want commit error", err)
	}
}

func seedMemoryCopy() ctxgraph.Copy {
	return ctxgraph.Copy{
		AgentID: "test",
		Graph: ctxgraph.Graph{
			Nodes: []ctxgraph.Node{
				{ID: "n0", Kind: ctxgraph.NodeKindFact, Statement: "zero", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"sg-a"}},
				{ID: "n1", Kind: ctxgraph.NodeKindFact, Statement: "one", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"sg-a"}},
				{ID: "n2", Kind: ctxgraph.NodeKindFact, Statement: "two", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"sg-a", "sg-b"}},
				{ID: "n3", Kind: ctxgraph.NodeKindFact, Statement: "three", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"sg-b"}},
				{ID: "n4", Kind: ctxgraph.NodeKindFact, Statement: "four", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"sg-b"}},
			},
			Edges: []ctxgraph.Edge{
				{FromRef: ctxgraph.NodeRef("n0"), ToNodeID: "n1", Kind: ctxgraph.EdgeKindLogicalAdjacent},
				{FromRef: ctxgraph.NodeRef("n1"), ToNodeID: "n2", Kind: ctxgraph.EdgeKindLogicalAdjacent},
				{FromRef: ctxgraph.NodeRef("n2"), ToNodeID: "n3", Kind: ctxgraph.EdgeKindLogicalAdjacent},
				{FromRef: ctxgraph.NodeRef("n3"), ToNodeID: "n4", Kind: ctxgraph.EdgeKindLogicalAdjacent},
				{FromRef: ctxgraph.SubgraphRef("sg-src"), ToNodeID: "n2", Kind: ctxgraph.EdgeKindDerivesFromSubgraph},
				{FromRef: ctxgraph.SubgraphRef("sg-a"), ToNodeID: "n2", Kind: ctxgraph.EdgeKindDerivesFromSubgraph},
			},
		},
	}
}

func executeMemory(t *testing.T, copy *ctxgraph.Copy, name, arguments string) (Output, error) {
	t.Helper()
	for _, tool := range MemoryTools(
		func() ctxgraph.Copy { return *copy },
		func(updated ctxgraph.Copy) { *copy = updated },
	) {
		if tool.Definition().Name != name {
			continue
		}
		return tool.Execute(context.Background(), Call{
			ID:        "call-1",
			Name:      name,
			Arguments: json.RawMessage(arguments),
		})
	}
	t.Fatalf("tool %q not registered", name)
	return Output{}, nil
}

func nodeIDs(nodes []memoryNode) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}
