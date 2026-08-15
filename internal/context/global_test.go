package context

import "testing"

func TestCloneAndUpdateIsolateGlobalGraph(t *testing.T) {
	t.Cleanup(func() { Update(Copy{}) })
	Update(Copy{})

	Update(Copy{
		AgentID: "agent-a",
		Graph: Graph{
			Nodes: []Node{{ID: "n1", Statement: "old"}},
		},
	})
	first := Clone("agent-a")
	if first.AgentID != "agent-a" {
		t.Fatalf("clone agent id = %q, want agent-a", first.AgentID)
	}
	Update(Copy{
		AgentID: "agent-b",
		Graph: Graph{
			Nodes: []Node{{ID: "n1", Statement: "new"}},
		},
	})
	second := Clone("agent-b")
	if second.AgentID != "agent-b" {
		t.Fatalf("clone agent id = %q, want agent-b", second.AgentID)
	}

	if len(first.Graph.Nodes) != 1 || first.Graph.Nodes[0].Statement != "old" {
		t.Fatalf("first clone = %#v, want old", first.Graph.Nodes)
	}
	if len(second.Graph.Nodes) != 1 || second.Graph.Nodes[0].Statement != "new" {
		t.Fatalf("second clone = %#v, want new", second.Graph.Nodes)
	}

	first.Graph.Nodes[0].Statement = "mutated"
	if Clone("agent-a").Graph.Nodes[0].Statement != "new" {
		t.Fatal("mutating a clone changed the global graph")
	}

	second.Graph.Nodes[0].Statement = "mutated again"
	Update(second)
	if Clone("agent-a").Graph.Nodes[0].Statement != "mutated again" {
		t.Fatal("update from clone did not replace the global graph")
	}
}
