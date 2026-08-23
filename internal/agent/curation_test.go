package agent

import (
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestCurationConfigNormalized(t *testing.T) {
	tests := []struct {
		name  string
		in    CurationConfig
		want  CurationConfig
		valid bool
	}{
		{name: "zero defaults", in: CurationConfig{}, want: CurationConfig{64, 32}, valid: true},
		{name: "explicit kept", in: CurationConfig{10, 5}, want: CurationConfig{10, 5}, valid: true},
		{name: "negative passes through for Validate", in: CurationConfig{DeepAuditMaxNodes: -1}, want: CurationConfig{DeepAuditMaxNodes: -1, DeepAuditMinAdded: 32}, valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.Normalized()
			if got != tt.want {
				t.Errorf("Normalized() = %+v, want %+v", got, tt.want)
			}
			err := tt.in.Validate()
			if tt.valid && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tt.valid && err == nil {
				t.Errorf("Validate() = nil, want error")
			}
		})
	}
}

func TestShouldDeepCurate(t *testing.T) {
	config := CurationConfig{DeepAuditMaxNodes: 64, DeepAuditMinAdded: 32}
	tests := []struct {
		name  string
		total int
		added int
		want  bool
	}{
		{name: "below both thresholds", total: 30, added: 5, want: false},
		{name: "total crosses", total: 64, added: 1, want: true},
		{name: "added crosses", total: 10, added: 32, want: true},
		{name: "added negative on unbound memory", total: 10, added: -1, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldDeepCurate(config, tt.total, tt.added); got != tt.want {
				t.Errorf("shouldDeepCurate(%d,%d) = %v, want %v", tt.total, tt.added, got, tt.want)
			}
		})
	}
}

func TestDeepCurationQueryListsAllNodes(t *testing.T) {
	snapshot := ctxgraph.Graph{Nodes: []ctxgraph.Node{
		{ID: "mem-1", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusDisputed, Statement: "无证据的已通过", CreatorAgentID: "task-1:executor"},
		{ID: "task-info-task-1", Kind: ctxgraph.NodeKindDirective, Status: ctxgraph.NodeStatusAccepted, Statement: "实现 X"},
	}}
	query := deepCurationQuery(snapshot)
	for _, want := range []string{
		"深度整理请求",
		"memory_apply",
		"- mem-1 [fact/disputed]（task-1:executor）无证据的已通过",
		"- task-info-task-1 [directive/accepted]（unknown）实现 X",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}
}

func TestDeepCurationQueryBoundsLargeGraph(t *testing.T) {
	snapshot := ctxgraph.Graph{Nodes: []ctxgraph.Node{
		{ID: "head-evidence", Kind: ctxgraph.NodeKindFact, Statement: strings.Repeat("h", 12<<10)},
		{ID: "middle-evidence", Kind: ctxgraph.NodeKindFact, Statement: strings.Repeat("m", 12<<10)},
		{ID: "tail-evidence", Kind: ctxgraph.NodeKindFact, Statement: strings.Repeat("t", 12<<10)},
	}}

	query := deepCurationQuery(snapshot)
	if len(query) > maxOrganizePromptBytes {
		t.Fatalf("deep curation prompt = %d bytes, want <= %d", len(query), maxOrganizePromptBytes)
	}
	for _, want := range []string{"head-evidence", "tail-evidence", "middle omitted"} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q", want)
		}
	}
}
