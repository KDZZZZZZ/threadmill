package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
)

type applyFixture struct {
	tool memoryApplyTool
	view *fakeMemoryView
}

func newApplyFixture(t *testing.T) applyFixture {
	t.Helper()
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a"}, {ID: "sg-b"}},
		Nodes: []ctxgraph.Node{
			{ID: "mem-1", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "无证据的已通过声明", SubgraphIDs: []string{"sg-a"}, CreatorAgentID: "task-1:executor"},
			{ID: "mem-2", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "pytest tests/test_x.py 退出码 0", SubgraphIDs: []string{"sg-a"}, CreatorAgentID: "task-1:verifier"},
			{ID: "task-info-task-1", Kind: ctxgraph.NodeKindDirective, Status: ctxgraph.NodeStatusAccepted, Statement: "实现 X", CreatorAgentID: "system"},
			{ID: "task-report-task-1", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "[Task Report] task-1: 结论: PASS", CreatorAgentID: "task-1:verifier"},
			{ID: "mem-3", Kind: ctxgraph.NodeKindDirective, Status: ctxgraph.NodeStatusAccepted, Statement: "进度按天汇总", CreatorAgentID: "task-1:planner"},
		},
	}
	view := &fakeMemoryView{graph: graph}
	return applyFixture{
		tool: memoryApplyTool{}.BindEnv(env.Env{Memory: view}).(memoryApplyTool),
		view: view,
	}
}

func applyOps(t *testing.T, f applyFixture, ops ...memoryApplyOp) (memoryApplyResult, error) {
	t.Helper()
	raw, err := json.Marshal(memoryApplyArgs{Ops: ops})
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	out, err := f.tool.Execute(context.Background(), Call{ID: "c1", Name: MemoryApplyName, Arguments: raw})
	if err != nil {
		return memoryApplyResult{}, err
	}
	var result memoryApplyResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("unmarshal result %q: %v", out.Content, err)
	}
	return result, nil
}

func TestMemoryApplyAppliesAuditBatch(t *testing.T) {
	f := newApplyFixture(t)
	result, err := applyOps(t, f,
		memoryApplyOp{Action: "status", ID: "mem-1", Status: "disputed", Reason: "缺证据锚"},
		memoryApplyOp{Action: "status", ID: "mem-2", Status: "superseded", SupersededBy: "mem-1", Reason: "被 mem-1 取代"},
		memoryApplyOp{Action: "create", Kind: "fact", Statement: "合并后的事实", SubgraphIDs: []string{"sg-b"}, Reason: "合并去重"},
		memoryApplyOp{Action: "delete", ID: "mem-3", Reason: ""},
	)
	if err == nil {
		t.Fatalf("expected reason-required error for the delete op, got %+v", result)
	}
	if !strings.Contains(err.Error(), "reason is required") {
		t.Errorf("err = %v, want reason required", err)
	}
	// 整批原子：失败时前两条也不能生效。
	for _, node := range f.view.graph.Nodes {
		if node.ID == "mem-1" && node.Status != ctxgraph.NodeStatusAccepted {
			t.Errorf("mem-1 status = %q, batch should not have committed", node.Status)
		}
	}
}

func TestMemoryApplySuccessfulBatch(t *testing.T) {
	f := newApplyFixture(t)
	result, err := applyOps(t, f,
		memoryApplyOp{Action: "status", ID: "mem-1", Status: "disputed", Reason: "缺证据锚：无命令引用"},
		memoryApplyOp{Action: "create", Kind: "fact", Statement: "合并节点", SubgraphIDs: []string{"sg-b"}, Reason: "合并去重"},
	)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Applied != 2 || len(result.CreatedIDs) != 1 {
		t.Fatalf("result = %+v", result)
	}
	node, ok := graphNode(f.view.graph, result.CreatedIDs[0])
	if !ok || node.Statement != "合并节点" {
		t.Errorf("created node missing: %+v", node)
	}
	if n, _ := graphNode(f.view.graph, "mem-1"); n.Status != ctxgraph.NodeStatusDisputed {
		t.Errorf("mem-1 status = %q", n.Status)
	}
}

func TestMemoryApplyGuardLayer(t *testing.T) {
	tests := []struct {
		name string
		op   memoryApplyOp
		want string
	}{
		{
			name: "task info node is fully protected",
			op:   memoryApplyOp{Action: "status", ID: "task-info-task-1", Status: "disputed", Reason: "r"},
			want: "contract memory",
		},
		{
			name: "report transcript rejects update",
			op:   memoryApplyOp{Action: "update", ID: "task-report-task-1", Statement: "改写", Reason: "r"},
			want: "only status changes are allowed",
		},
		{
			name: "report transcript rejects delete",
			op:   memoryApplyOp{Action: "delete", ID: "task-report-task-1", Reason: "r"},
			want: "only status changes are allowed",
		},
		{
			name: "agent directive rejects delete",
			op:   memoryApplyOp{Action: "delete", ID: "mem-3", Reason: "r"},
			want: "only status changes are allowed",
		},
		{
			name: "agent directive rejects statement rewrite",
			op:   memoryApplyOp{Action: "update", ID: "mem-3", Statement: "改写", Reason: "r"},
			want: "only status changes are allowed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newApplyFixture(t)
			_, err := applyOps(t, f, tt.op)
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestMemoryApplyReportStatusChangeAllowed(t *testing.T) {
	f := newApplyFixture(t)
	if _, err := applyOps(t, f,
		memoryApplyOp{Action: "status", ID: "task-report-task-1", Status: "disputed", Reason: "PASS 无命令证据"},
	); err != nil {
		t.Fatalf("status on report transcript should be allowed: %v", err)
	}
	node, _ := graphNode(f.view.graph, "task-report-task-1")
	if node.Status != ctxgraph.NodeStatusDisputed {
		t.Errorf("status = %q, want disputed", node.Status)
	}
}

type fakeMemoryView struct {
	graph ctxgraph.Graph
}

func (v *fakeMemoryView) Snapshot() ctxgraph.Graph { return v.graph.Clone() }
func (v *fakeMemoryView) Commit(g ctxgraph.Graph) error {
	v.graph = g.Clone()
	return nil
}
