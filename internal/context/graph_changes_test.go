package context

import (
	"strings"
	"testing"
)

func changesFixtureGraph() Graph {
	return Graph{
		Subgraphs: []Subgraph{{ID: "sg-a"}, {ID: "sg-b"}},
		Nodes: []Node{
			{ID: "mem-1", Kind: NodeKindFact, Status: NodeStatusAccepted, Statement: "pytest 退出码 0", SubgraphIDs: []string{"sg-a"}, CreatorAgentID: "task-1:verifier"},
			{ID: "task-info-task-1", Kind: NodeKindDirective, Status: NodeStatusAccepted, Statement: "实现 X", CreatorAgentID: "system"},
		},
	}
}

func TestWithNodeChangesAppliesFullBatch(t *testing.T) {
	tests := []struct {
		name    string
		changes []NodeChange
		check   func(t *testing.T, before, after Graph)
	}{
		{
			name: "create assigns id and membership",
			changes: []NodeChange{{
				Action: NodeChangeCreate, Kind: NodeKindFact,
				Statement: "新事实", CreatorAgentID: "subgraph-organizer", SubgraphIDs: []string{"sg-a", "missing"},
			}},
			check: func(t *testing.T, before, after Graph) {
				node, ok := after.nodeByID("mem-2")
				if !ok {
					t.Fatalf("expected auto id mem-2, nodes: %v", after.Nodes)
				}
				if node.CreatorAgentID != "subgraph-organizer" {
					t.Errorf("creator = %q", node.CreatorAgentID)
				}
				if len(node.SubgraphIDs) != 1 || node.SubgraphIDs[0] != "sg-a" {
					t.Errorf("membership = %v, want only known sg-a", node.SubgraphIDs)
				}
			},
		},
		{
			name: "update replaces statement and kind",
			changes: []NodeChange{{
				Action: NodeChangeUpdate, ID: "mem-1", Statement: "改写后的陈述", Kind: NodeKindHypothesis,
			}},
			check: func(t *testing.T, _, after Graph) {
				node, _ := after.nodeByID("mem-1")
				if node.Statement != "改写后的陈述" || node.Kind != NodeKindHypothesis {
					t.Errorf("node = %+v", node)
				}
			},
		},
		{
			name: "status records superseded_by",
			changes: []NodeChange{{
				Action: NodeChangeStatus, ID: "mem-1", Status: NodeStatusSuperseded, SupersededBy: "task-info-task-1",
			}},
			check: func(t *testing.T, _, after Graph) {
				node, _ := after.nodeByID("mem-1")
				if node.Status != NodeStatusSuperseded || node.SupersededBy != "task-info-task-1" {
					t.Errorf("node = %+v", node)
				}
			},
		},
		{
			name: "status without superseded_by clears field",
			changes: []NodeChange{
				{Action: NodeChangeStatus, ID: "mem-1", Status: NodeStatusSuperseded, SupersededBy: "task-info-task-1"},
				{Action: NodeChangeStatus, ID: "mem-1", Status: NodeStatusDisputed},
			},
			check: func(t *testing.T, _, after Graph) {
				node, _ := after.nodeByID("mem-1")
				if node.SupersededBy != "" {
					t.Errorf("superseded_by = %q, want cleared", node.SupersededBy)
				}
			},
		},
		{
			name: "delete removes node and its edges",
			changes: []NodeChange{
				{Action: NodeChangeCreate, Kind: NodeKindFact, Statement: "bridge", ID: "mem-9"},
				{Action: NodeChangeDelete, ID: "mem-1"},
			},
			check: func(t *testing.T, _, after Graph) {
				if _, ok := after.nodeByID("mem-1"); ok {
					t.Errorf("mem-1 still present")
				}
			},
		},
		{
			name: "attach and detach adjust membership",
			changes: []NodeChange{
				{Action: NodeChangeAttach, ID: "mem-1", SubgraphIDs: []string{"sg-b"}},
				{Action: NodeChangeDetach, ID: "mem-1", SubgraphIDs: []string{"sg-a"}},
			},
			check: func(t *testing.T, _, after Graph) {
				node, _ := after.nodeByID("mem-1")
				if len(node.SubgraphIDs) != 1 || node.SubgraphIDs[0] != "sg-b" {
					t.Errorf("membership = %v, want [sg-b]", node.SubgraphIDs)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := changesFixtureGraph()
			after, err := before.WithNodeChanges(tt.changes)
			if err != nil {
				t.Fatalf("WithNodeChanges: %v", err)
			}
			if after.Revision <= before.Revision {
				t.Errorf("revision = %d, want > %d", after.Revision, before.Revision)
			}
			tt.check(t, before, after)
		})
	}
}

func TestWithNodeChangesRejectsInvalidBatchAtomically(t *testing.T) {
	tests := []struct {
		name    string
		changes []NodeChange
		wantErr string
	}{
		{
			name:    "unknown action",
			changes: []NodeChange{{Action: "rename", ID: "mem-1"}},
			wantErr: "node change 1",
		},
		{
			name: "unknown target keeps earlier ops unapplied",
			changes: []NodeChange{
				{Action: NodeChangeDelete, ID: "mem-1"},
				{Action: NodeChangeStatus, ID: "ghost", Status: NodeStatusDisputed},
			},
			wantErr: "node change 2",
		},
		{
			name:    "create without statement",
			changes: []NodeChange{{Action: NodeChangeCreate, Kind: NodeKindFact}},
			wantErr: "statement is required",
		},
		{
			name:    "create with invalid kind",
			changes: []NodeChange{{Action: NodeChangeCreate, Kind: "rumor", Statement: "s"}},
			wantErr: "invalid kind",
		},
		{
			name:    "status with invalid value",
			changes: []NodeChange{{Action: NodeChangeStatus, ID: "mem-1", Status: "maybe"}},
			wantErr: "invalid status",
		},
		{
			name: "status self supersede",
			changes: []NodeChange{{
				Action: NodeChangeStatus, ID: "mem-1", Status: NodeStatusSuperseded, SupersededBy: "mem-1",
			}},
			wantErr: "cannot supersede itself",
		},
		{
			name:    "update without statement",
			changes: []NodeChange{{Action: NodeChangeUpdate, ID: "mem-1"}},
			wantErr: "full replacement",
		},
		{
			name:    "attach without known subgraphs",
			changes: []NodeChange{{Action: NodeChangeAttach, ID: "mem-1", SubgraphIDs: []string{"nope"}}},
			wantErr: "no known subgraph",
		},
		{
			name:    "duplicate create id",
			changes: []NodeChange{{Action: NodeChangeCreate, ID: "mem-1", Kind: NodeKindFact, Statement: "dup"}},
			wantErr: "already exists",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := changesFixtureGraph()
			after, err := before.WithNodeChanges(tt.changes)
			if err == nil {
				t.Fatalf("expected error containing %q, got graph", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tt.wantErr)
			}
			if len(after.Nodes) != 0 {
				t.Errorf("expected zero graph on failure, got %d nodes", len(after.Nodes))
			}
			if len(before.Nodes) != 2 {
				t.Errorf("input graph mutated: %d nodes", len(before.Nodes))
			}
		})
	}
}
