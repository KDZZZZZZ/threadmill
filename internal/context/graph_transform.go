package context

import (
	"fmt"
	"slices"
)

func (g Graph) withRuntimeNode(subgraph Subgraph, node Node) (Graph, error) {
	out := g.Clone()
	if !hasSubgraphID(out, subgraph.ID) {
		out = out.WithSubgraph(subgraph)
	}
	if node.ID == "" {
		node.ID = nextSystemNodeID(out)
	}
	node.SubgraphIDs = []string{subgraph.ID}
	for i, existing := range out.Nodes {
		if existing.ID != node.ID {
			continue
		}
		if !containsID(existing.SubgraphIDs, subgraph.ID) {
			return Graph{}, fmt.Errorf("context: node ID %q already exists outside target subgraph", node.ID)
		}
		if sameNode(existing, node) {
			return out, nil
		}
		out.Nodes[i] = cloneNode(node)
		out.Revision++
		for j := range out.Subgraphs {
			if out.Subgraphs[j].ID == subgraph.ID {
				out.Subgraphs[j].Revision++
			}
		}
		return out, nil
	}
	return out.WithMemory([]Node{node}, nil), nil
}

func (g Graph) withoutSubgraph(subgraphID string) Graph {
	out := g.Clone()
	removed := make(map[string]struct{})
	nodes := out.Nodes[:0]
	for _, node := range out.Nodes {
		if containsID(node.SubgraphIDs, subgraphID) {
			removed[node.ID] = struct{}{}
			continue
		}
		nodes = append(nodes, node)
	}
	subgraphs := out.Subgraphs[:0]
	for _, subgraph := range out.Subgraphs {
		if subgraph.ID != subgraphID {
			subgraphs = append(subgraphs, subgraph)
		}
	}
	edges := out.Edges[:0]
	for _, edge := range out.Edges {
		if _, drop := removed[edge.ToNodeID]; drop {
			continue
		}
		if id, ok := parseRef(edge.FromRef, nodeRefPrefix); ok {
			if _, drop := removed[id]; drop {
				continue
			}
		}
		if id, ok := parseRef(edge.FromRef, subgraphRefPrefix); ok && id == subgraphID {
			continue
		}
		edges = append(edges, edge)
	}
	out.Nodes = nodes
	out.Subgraphs = subgraphs
	out.Edges = edges
	out.Revision++
	return out
}

func (current Graph) preservingManaged(next Graph) Graph {
	managed := make(map[string]struct{})
	system := make(map[string]struct{})
	for _, subgraph := range current.Subgraphs {
		if subgraph.Kind == SubgraphKindSystem || subgraph.Kind == SubgraphKindPackage {
			managed[subgraph.ID] = struct{}{}
		}
		if subgraph.Kind == SubgraphKindSystem {
			system[subgraph.ID] = struct{}{}
		}
	}
	for _, subgraph := range next.Subgraphs {
		if subgraph.Kind == SubgraphKindSystem {
			system[subgraph.ID] = struct{}{}
		}
	}
	if len(managed) == 0 {
		return next.Clone()
	}

	out := next.Clone()
	subgraphs := out.Subgraphs[:0]
	for _, subgraph := range out.Subgraphs {
		if _, ok := managed[subgraph.ID]; !ok {
			subgraphs = append(subgraphs, subgraph)
		}
	}
	for _, subgraph := range current.Subgraphs {
		if _, ok := managed[subgraph.ID]; ok {
			subgraphs = append(subgraphs, subgraph)
		}
	}
	out.Subgraphs = subgraphs

	owned := make(map[string]Node)
	for _, node := range current.Nodes {
		for _, id := range node.SubgraphIDs {
			if _, ok := managed[id]; ok {
				owned[node.ID] = node
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(owned))
	for i, node := range out.Nodes {
		if kept, ok := owned[node.ID]; ok {
			kept = cloneNode(kept)
			for _, id := range node.SubgraphIDs {
				if _, protected := system[id]; protected {
					continue
				}
				kept.SubgraphIDs = unionIDs(kept.SubgraphIDs, []string{id})
			}
			out.Nodes[i] = kept
			seen[node.ID] = struct{}{}
			continue
		}
		ids := node.SubgraphIDs[:0]
		for _, id := range node.SubgraphIDs {
			if _, protected := system[id]; !protected {
				ids = append(ids, id)
			}
		}
		out.Nodes[i].SubgraphIDs = ids
	}
	for _, node := range current.Nodes {
		if _, ok := owned[node.ID]; !ok {
			continue
		}
		if _, ok := seen[node.ID]; ok {
			continue
		}
		out.Nodes = append(out.Nodes, cloneNode(node))
	}
	if current.Revision > out.Revision {
		out.Revision = current.Revision
	}
	return out
}

// mergeAdditive 以 g 为底，并入 theirs 相对 base 的节点、子图和边，Revision 加一。
//
// additive-only 不变量（join 回流的合同，测试在 merge_test.go 锁定）：合入只允许两件事——
// ① 新增节点、子图和边；② 同 ID 同 statement 节点的 SubgraphIDs 归属并集（附着不算修改内容）。
// 除归属并集外，g 中已有节点的 statement、kind、status、source_refs、creator_agent_id 和
// superseded_by 永不被 theirs 改写：child 改 statement 会保留 g 的原节点、把 child 版本重映射成
// 新 ID 后新增；child 改 status 直接跳过；child 的删除永不传播。已有子图的元数据（含
// name/summary/admission/scope）同样不被覆盖，只有 g 中不存在的子图才整条追加。
func (g Graph) mergeAdditive(fromID string, base, theirs Graph) Graph {
	result := g.Clone()
	remap := make(map[string]string)
	used := make(map[string]struct{})
	for _, node := range result.Nodes {
		if node.ID != "" {
			used[node.ID] = struct{}{}
		}
	}
	for _, node := range theirs.Nodes {
		if node.ID != "" {
			used[node.ID] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	for _, node := range theirs.Nodes {
		if node.ID == "" {
			continue
		}
		if _, dup := seen[node.ID]; dup {
			continue
		}
		seen[node.ID] = struct{}{}

		oursNode, oursOK := result.nodeByID(node.ID)
		if oursOK && oursNode.Statement == node.Statement {
			for i := range result.Nodes {
				if result.Nodes[i].ID != node.ID {
					continue
				}
				result.Nodes[i].SubgraphIDs = unionIDs(result.Nodes[i].SubgraphIDs, node.SubgraphIDs)
				break
			}
			continue
		}
		if baseNode, ok := base.nodeByID(node.ID); ok && baseNode.Statement == node.Statement {
			continue
		}
		if oursOK {
			newID, existed := collisionNodeID(fromID, node, result, used)
			remap[node.ID] = newID
			if existed {
				continue
			}
			cloned := cloneNode(node)
			cloned.ID = newID
			result.Nodes = append(result.Nodes, cloned)
			used[newID] = struct{}{}
			continue
		}
		result.Nodes = append(result.Nodes, cloneNode(node))
		used[node.ID] = struct{}{}
	}

	for _, subgraph := range theirs.Subgraphs {
		if subgraph.ID == "" || hasSubgraphID(result, subgraph.ID) {
			continue
		}
		result.Subgraphs = append(result.Subgraphs, subgraph)
	}

	for _, edge := range theirs.Edges {
		if hasEdge(base, edge) {
			continue
		}
		edge = rewriteEdge(edge, remap)
		if hasEdge(result, edge) {
			continue
		}
		result.Edges = append(result.Edges, edge)
	}

	result.Revision = g.Revision + 1
	return result
}

func nextSystemNodeID(graph Graph) string {
	for next := len(graph.Nodes) + 1; ; next++ {
		id := fmt.Sprintf("system-%d", next)
		if _, exists := graph.nodeByID(id); !exists {
			return id
		}
	}
}

func sameNode(a, b Node) bool {
	return a.ID == b.ID &&
		a.Kind == b.Kind &&
		a.Statement == b.Statement &&
		a.Status == b.Status &&
		a.CreatorAgentID == b.CreatorAgentID &&
		a.SupersededBy == b.SupersededBy &&
		slices.Equal(a.SubgraphIDs, b.SubgraphIDs) &&
		slices.Equal(a.SourceRefs, b.SourceRefs)
}

func collisionNodeID(fromID string, node Node, result Graph, used map[string]struct{}) (string, bool) {
	preferred := fromID + "-" + node.ID
	if preferred != node.ID {
		if existing, ok := result.nodeByID(preferred); ok {
			if existing.Statement == node.Statement {
				return preferred, true
			}
		} else {
			return preferred, false
		}
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("mem-%d", i)
		if _, taken := used[id]; taken {
			continue
		}
		return id, false
	}
}

func unionIDs(dst, extra []string) []string {
	out := append([]string(nil), dst...)
	for _, id := range extra {
		if id == "" || containsID(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

func rewriteEdge(edge Edge, remap map[string]string) Edge {
	if newID, ok := remap[edge.ToNodeID]; ok {
		edge.ToNodeID = newID
	}
	if oldID, ok := parseRef(edge.FromRef, nodeRefPrefix); ok {
		if newID, ok := remap[oldID]; ok {
			edge.FromRef = NodeRef(newID)
		}
	}
	return edge
}

func hasSubgraphID(g Graph, id string) bool {
	for _, subgraph := range g.Subgraphs {
		if subgraph.ID == id {
			return true
		}
	}
	return false
}

func hasEdge(g Graph, want Edge) bool {
	for _, edge := range g.Edges {
		if edge == want {
			return true
		}
	}
	return false
}
