package agent

import (
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func viewFixture() ctxgraph.Graph {
	return ctxgraph.Graph{
		Revision: 4,
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-a", Name: "包 A", Summary: "任务启动包", Revision: 2, Kind: "package"},
			{ID: "sg-b", Name: "包 B", Summary: "主题子图", Admission: "只收带命令与退出码的 fact", Scope: "verifier 验收阶段", Revision: 3, Kind: "general"},
		},
		Nodes: []ctxgraph.Node{
			{ID: "n-1", Kind: "fact", Statement: "命令 go test 退出码 0", Status: "accepted", SubgraphIDs: []string{"sg-a"}, SourceRefs: []string{"src-1"}, CreatorAgentID: "task-1:executor"},
			{ID: "n-2", Kind: "directive", Statement: "必须保留公共 API", Status: "accepted", SubgraphIDs: []string{"sg-a", "sg-b"}, CreatorAgentID: "manager"},
			{ID: "n-3", Kind: "hypothesis", Statement: "疑似缓存未失效", Status: "disputed", CreatorAgentID: "task-2:planner"},
		},
	}
}

func subgraphByID(t *testing.T, view memoryView, id string) memoryViewSubgraph {
	t.Helper()
	for _, subgraph := range view.Subgraphs {
		if subgraph.ID == id {
			return subgraph
		}
	}
	t.Fatalf("subgraph %q missing from view", id)
	return memoryViewSubgraph{}
}

func TestBuildMemoryViewLevelOneKeepsSubgraphFieldsOnly(t *testing.T) {
	view := buildMemoryView(viewFixture(), memoryLevelSubgraph, nil)

	got := subgraphByID(t, view, "sg-a")
	if got.Name != "包 A" || got.Summary != "任务启动包" || got.Revision != 2 || got.Kind != "package" {
		t.Fatalf("level 1 dropped subgraph fields: %+v", got)
	}
	if got.NodeCount != 2 {
		t.Errorf("NodeCount = %d, want 2", got.NodeCount)
	}
	if len(got.Nodes) != 0 {
		t.Errorf("level 1 leaked %d nodes", len(got.Nodes))
	}
	if view.UnassignedCount != 1 || len(view.Unassigned) != 0 {
		t.Errorf("unassigned = %d/%d, want count 1 and no detail", view.UnassignedCount, len(view.Unassigned))
	}

	// 子图说明是级别 1 的内容：所有 Agent 都要能看到准入内容和适用范围，
	// 否则无从判断该往哪张子图放节点、该不该继续订阅它。
	described := subgraphByID(t, view, "sg-b")
	if described.Admission != "只收带命令与退出码的 fact" || described.Scope != "verifier 验收阶段" {
		t.Fatalf("level 1 dropped the subgraph description: %+v", described)
	}
}

func TestBuildMemoryViewLevelTwoHidesStatementOnly(t *testing.T) {
	view := buildMemoryView(viewFixture(), memoryLevelNode, nil)

	got := subgraphByID(t, view, "sg-a")
	if len(got.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2", len(got.Nodes))
	}
	for _, node := range got.Nodes {
		if node.Statement != "" {
			t.Errorf("node %s leaked statement at level 2", node.ID)
		}
		if node.Kind == "" || node.Status == "" || node.CreatorAgentID == "" {
			t.Errorf("node %s lost a non-statement field: %+v", node.ID, node)
		}
	}
	if got.Nodes[0].SourceRefs == nil {
		t.Error("level 2 dropped source_refs")
	}
}

func TestBuildMemoryViewLevelThreeIncludesStatement(t *testing.T) {
	view := buildMemoryView(viewFixture(), memoryLevelFull, nil)

	got := subgraphByID(t, view, "sg-a")
	for _, node := range got.Nodes {
		if node.Statement == "" {
			t.Errorf("node %s missing statement at level 3", node.ID)
		}
	}
	if len(view.Unassigned) != 1 || view.Unassigned[0].Statement == "" {
		t.Errorf("unassigned node not expanded at level 3: %+v", view.Unassigned)
	}
}

func TestBuildMemoryViewPerTargetOverrideBeatsDefault(t *testing.T) {
	levels := map[string]int{"sg-b": memoryLevelFull, "n-1": memoryLevelNode}
	view := buildMemoryView(viewFixture(), memoryLevelSubgraph, levels)

	a := subgraphByID(t, view, "sg-a")
	if len(a.Nodes) != 1 || a.Nodes[0].ID != "n-1" {
		t.Fatalf("sg-a should surface only the overridden n-1, got %+v", a.Nodes)
	}
	if a.Nodes[0].Statement != "" {
		t.Error("n-1 override was level 2 but statement leaked")
	}

	b := subgraphByID(t, view, "sg-b")
	if len(b.Nodes) != 1 || b.Nodes[0].Statement == "" {
		t.Fatalf("sg-b expanded to level 3 should carry statements, got %+v", b.Nodes)
	}
}

func TestEffectiveNodeLevelTakesHighestSubgraph(t *testing.T) {
	node := ctxgraph.Node{ID: "n-2", SubgraphIDs: []string{"sg-a", "sg-b"}}
	levels := map[string]int{"sg-a": memoryLevelSubgraph, "sg-b": memoryLevelFull}

	if got := effectiveNodeLevel(node, memoryLevelSubgraph, levels); got != memoryLevelFull {
		t.Fatalf("effectiveNodeLevel = %d, want %d", got, memoryLevelFull)
	}
}

func TestSetMemoryLevelsCollapseAllClearsHigherOverrides(t *testing.T) {
	loop := &Loop{}
	loop.setMemoryLevels([]string{"sg-a", "n-1"}, memoryLevelFull, false)

	loop.setMemoryLevels(nil, memoryLevelSubgraph, true)

	def, levels := loop.memoryLevels()
	if def != memoryLevelSubgraph {
		t.Fatalf("default = %d, want %d", def, memoryLevelSubgraph)
	}
	if len(levels) != 0 {
		t.Fatalf("collapse-all left overrides: %v", levels)
	}
}

func TestSetMemoryLevelsExpandAllKeepsOverrides(t *testing.T) {
	loop := &Loop{}
	loop.setMemoryLevels([]string{"n-1"}, memoryLevelFull, false)

	loop.setMemoryLevels(nil, memoryLevelNode, false)

	_, levels := loop.memoryLevels()
	if levels["n-1"] != memoryLevelFull {
		t.Fatalf("expand-all dropped explicit override: %v", levels)
	}
}

func TestMemoryLevelsDefaultsToSubgraph(t *testing.T) {
	loop := &Loop{}

	def, levels := loop.memoryLevels()
	if def != memoryLevelSubgraph {
		t.Fatalf("default = %d, want %d", def, memoryLevelSubgraph)
	}
	if len(levels) != 0 {
		t.Fatalf("fresh loop has overrides: %v", levels)
	}
}

func TestCollapsedNodeIDsSkipsStillExpandedNodes(t *testing.T) {
	levels := map[string]int{"n-1": memoryLevelFull}

	got := collapsedNodeIDs(viewFixture(), memoryLevelSubgraph, levels)

	if _, ok := got["n-1"]; ok {
		t.Error("n-1 is still at level 3 and must not be stripped from history")
	}
	for _, id := range []string{"n-2", "n-3"} {
		if _, ok := got[id]; !ok {
			t.Errorf("collapsed node %s missing from strip set", id)
		}
	}
}

func TestValidateTargetsRejectsUnknownID(t *testing.T) {
	if err := validateTargets("memory_expand", viewFixture(), []string{"sg-a", "nope"}); err == nil {
		t.Fatal("unknown target accepted")
	}
	if err := validateTargets("memory_expand", viewFixture(), []string{"sg-a", "n-3"}); err != nil {
		t.Fatalf("known targets rejected: %v", err)
	}
}

func TestResolveLevelBounds(t *testing.T) {
	expand := memoryViewTool{}
	collapse := memoryViewTool{collapse: true}
	three := memoryLevelFull

	if got, err := expand.resolveLevel(nil); err != nil || got != memoryLevelFull {
		t.Fatalf("expand default = %d, %v; want %d", got, err, memoryLevelFull)
	}
	if got, err := collapse.resolveLevel(nil); err != nil || got != memoryLevelSubgraph {
		t.Fatalf("collapse default = %d, %v; want %d", got, err, memoryLevelSubgraph)
	}
	if _, err := collapse.resolveLevel(&three); err == nil {
		t.Fatal("collapse accepted level 3")
	}
}
