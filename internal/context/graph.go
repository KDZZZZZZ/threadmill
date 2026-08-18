// Package context 定义 Threadmill 的上下文图对象。
//
// 图是某一 revision 的快照：子图只保存元数据，节点通过 SubgraphIDs 声明正式归属，
// 边用 FromRef（node:<id> 或 subgraph:<id>）表达来源子图与节点邻接。
package context

import "strings"

// 节点 Kind 描述陈述的语义，不决定它属于哪个子图。
const (
	NodeKindDirective  = "directive"  // 约束、要求或稳定偏好
	NodeKindFact       = "fact"       // 已成立或已被接受的事实
	NodeKindHypothesis = "hypothesis" // 尚待验证的推测
)

// 节点 Status 描述当前有效性；历史节点通过状态变化保留在图中。
const (
	NodeStatusAccepted   = "accepted"
	NodeStatusDisputed   = "disputed"
	NodeStatusSuperseded = "superseded"
	NodeStatusOutdated   = "outdated"
)

// 边 Kind 只表达图结构关系，不承载额外业务状态。
const (
	EdgeKindLogicalAdjacent     = "logical_adjacent"      // 创建顺序上相邻的节点
	EdgeKindDerivesFromSubgraph = "derives_from_subgraph" // 在某个子图上下文中产生的节点
)

// 子图 Kind 区分通用知识与 Task 专属上下文。
const (
	SubgraphKindGeneral = "general"
	SubgraphKindTask    = "task"
)

// Node 是上下文图中的一条知识陈述。
type Node struct {
	ID             string   `json:"id"`               // 节点稳定标识
	Kind           string   `json:"kind"`             // directive、fact 或 hypothesis
	Statement      string   `json:"statement"`        // 原子化知识陈述
	Status         string   `json:"status"`           // 当前有效性
	SubgraphIDs    []string `json:"subgraph_ids"`     // 正式归属；一个节点可属于多个子图
	SourceRefs     []string `json:"source_refs"`      // 支撑该陈述的来源引用
	CreatorAgentID string   `json:"creator_agent_id"` // 创建该节点的 Agent
}

// Edge 是从节点或子图引用指向节点的有向关系。
type Edge struct {
	FromRef  string `json:"from_ref"`   // 来源引用，格式为 node:<id> 或 subgraph:<id>
	ToNodeID string `json:"to_node_id"` // 目标节点 ID
	Kind     string `json:"kind"`       // 关系种类
}

// Subgraph 是一组由节点归属关系组成的上下文视图。
type Subgraph struct {
	ID       string `json:"id"`       // 子图稳定标识
	Name     string `json:"name"`     // 面向 Agent 的名称
	Summary  string `json:"summary"`  // 用于列表和检索的简要说明
	Revision int64  `json:"revision"` // 子图内容版本
	Kind     string `json:"kind"`     // general 或 task
}

// Graph 是某一 revision 下的上下文图快照。
type Graph struct {
	Revision  int64      `json:"revision"`  // 整张图的版本
	Subgraphs []Subgraph `json:"subgraphs"` // 子图元数据；不重复保存节点 ID
	Nodes     []Node     `json:"nodes"`     // 节点及其子图归属
	Edges     []Edge     `json:"edges"`     // 节点与子图之间的有向关系
}

// NodesInSubgraphs 返回至少属于其中一个子图的节点并集。
// 按图中原有顺序且按 ID 去重；返回值为节点拷贝。订阅列表为空或全未知时返回空切片。
func (g Graph) NodesInSubgraphs(subgraphIDs []string) []Node {
	wanted := make(map[string]struct{}, len(subgraphIDs))
	for _, id := range subgraphIDs {
		if id == "" {
			continue
		}
		wanted[id] = struct{}{}
	}
	nodes := make([]Node, 0)
	if len(wanted) == 0 {
		return nodes
	}

	seen := make(map[string]struct{}, len(g.Nodes))
	for _, node := range g.Nodes {
		if node.ID != "" {
			if _, dup := seen[node.ID]; dup {
				continue
			}
		}
		if !nodeInSubgraphs(node, wanted) {
			continue
		}
		if node.ID != "" {
			seen[node.ID] = struct{}{}
		}
		nodes = append(nodes, cloneNode(node))
	}
	return nodes
}

// LastNodeOfCreator 返回该创建者在图中按 Nodes 顺序的最后一个节点。
// agentID 为空或没有匹配节点时 ok 为 false。
func (g Graph) LastNodeOfCreator(agentID string) (Node, bool) {
	if agentID == "" {
		return Node{}, false
	}
	for i := len(g.Nodes) - 1; i >= 0; i-- {
		if g.Nodes[i].CreatorAgentID == agentID {
			return cloneNode(g.Nodes[i]), true
		}
	}
	return Node{}, false
}

// SubgraphsOf 返回节点正式归属的子图 ID。
// 归属写在 Node.SubgraphIDs 上，与来源子图无关；节点不存在时返回空切片。
func (g Graph) SubgraphsOf(nodeID string) []string {
	ids := make([]string, 0)
	node, ok := g.nodeByID(nodeID)
	if !ok {
		return ids
	}

	seen := make(map[string]struct{}, len(node.SubgraphIDs))
	for _, id := range node.SubgraphIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// SourceSubgraphsOf 返回产生该节点的来源子图 ID。
// 只看 Kind 为 derives_from_subgraph、FromRef 为 subgraph:<id> 的入边，
// 与正式归属 SubgraphsOf 不同；节点不存在时返回空切片。
func (g Graph) SourceSubgraphsOf(nodeID string) []string {
	ids := make([]string, 0)
	if _, ok := g.nodeByID(nodeID); !ok {
		return ids
	}

	seen := make(map[string]struct{})
	for _, edge := range g.Edges {
		if edge.Kind != EdgeKindDerivesFromSubgraph || edge.ToNodeID != nodeID {
			continue
		}
		id, ok := parseRef(edge.FromRef, subgraphRefPrefix)
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// UpstreamNodes 返回指向该节点的上游节点。
// 只认 FromRef 为 node:<id> 的入边，子图引用不算上游；对端节点缺失则跳过。
// 按边的原有顺序且按 ID 去重，返回节点拷贝；节点不存在时返回空切片。
func (g Graph) UpstreamNodes(nodeID string) []Node {
	nodes := make([]Node, 0)
	if _, ok := g.nodeByID(nodeID); !ok {
		return nodes
	}

	seen := make(map[string]struct{})
	for _, edge := range g.Edges {
		if edge.ToNodeID != nodeID {
			continue
		}
		fromID, ok := parseRef(edge.FromRef, nodeRefPrefix)
		if !ok {
			continue
		}
		if _, dup := seen[fromID]; dup {
			continue
		}
		node, ok := g.nodeByID(fromID)
		if !ok {
			continue
		}
		seen[fromID] = struct{}{}
		nodes = append(nodes, cloneNode(node))
	}
	return nodes
}

// DownstreamNodes 返回该节点指向的下游节点。
// 只认出边 FromRef 为 node:<id> 的节点，对端节点缺失则跳过。
// 按边的原有顺序且按 ID 去重，返回节点拷贝；节点不存在时返回空切片。
func (g Graph) DownstreamNodes(nodeID string) []Node {
	nodes := make([]Node, 0)
	if _, ok := g.nodeByID(nodeID); !ok {
		return nodes
	}

	seen := make(map[string]struct{})
	for _, edge := range g.Edges {
		fromID, ok := parseRef(edge.FromRef, nodeRefPrefix)
		if !ok || fromID != nodeID {
			continue
		}
		if edge.ToNodeID == "" {
			continue
		}
		if _, dup := seen[edge.ToNodeID]; dup {
			continue
		}
		node, ok := g.nodeByID(edge.ToNodeID)
		if !ok {
			continue
		}
		seen[edge.ToNodeID] = struct{}{}
		nodes = append(nodes, cloneNode(node))
	}
	return nodes
}

// Clone 深拷贝图快照，避免调用方继续修改切片后影响已保存的版本。
func (g Graph) Clone() Graph {
	cloned := Graph{
		Revision:  g.Revision,
		Subgraphs: append([]Subgraph(nil), g.Subgraphs...),
		Nodes:     make([]Node, 0, len(g.Nodes)),
		Edges:     append([]Edge(nil), g.Edges...),
	}
	for _, node := range g.Nodes {
		cloned.Nodes = append(cloned.Nodes, cloneNode(node))
	}
	return cloned
}

// Edge.FromRef 的引用前缀：node:<id> 指向节点，subgraph:<id> 指向子图。
const (
	nodeRefPrefix     = "node:"
	subgraphRefPrefix = "subgraph:"
)

// NodeRef 构造指向节点的边来源引用。
func NodeRef(id string) string {
	return nodeRefPrefix + id
}

// SubgraphRef 构造指向子图的边来源引用。
func SubgraphRef(id string) string {
	return subgraphRefPrefix + id
}

// WithMemory 追加记忆节点和边，并增加图版本。
func (g Graph) WithMemory(nodes []Node, edges []Edge) Graph {
	if len(nodes) == 0 && len(edges) == 0 {
		return g.Clone()
	}
	cloned := g.Clone()
	for _, node := range nodes {
		cloned.Nodes = append(cloned.Nodes, cloneNode(node))
	}
	cloned.Edges = append(cloned.Edges, edges...)
	cloned.Revision++
	return cloned
}

// WithNodesInSubgraph 把指定节点加入子图（写入 Node.SubgraphIDs）。
// 未知节点跳过；已属于该子图的节点不变；有变更时图版本加一。
func (g Graph) WithNodesInSubgraph(subgraphID string, nodeIDs []string) Graph {
	if subgraphID == "" || len(nodeIDs) == 0 {
		return g.Clone()
	}

	wanted := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		if id == "" {
			continue
		}
		wanted[id] = struct{}{}
	}
	if len(wanted) == 0 {
		return g.Clone()
	}

	cloned := g.Clone()
	changed := false
	seen := make(map[string]struct{}, len(cloned.Nodes))
	for i, node := range cloned.Nodes {
		if node.ID == "" {
			continue
		}
		if _, dup := seen[node.ID]; dup {
			continue
		}
		seen[node.ID] = struct{}{}
		if _, ok := wanted[node.ID]; !ok {
			continue
		}
		if containsID(node.SubgraphIDs, subgraphID) {
			continue
		}
		cloned.Nodes[i].SubgraphIDs = append(cloned.Nodes[i].SubgraphIDs, subgraphID)
		changed = true
	}
	if !changed {
		return cloned
	}

	cloned.Revision++
	for i, subgraph := range cloned.Subgraphs {
		if subgraph.ID == subgraphID {
			cloned.Subgraphs[i].Revision++
		}
	}
	return cloned
}

// WithSubgraph 写入或替换子图元数据；ID 为空则只克隆。
func (g Graph) WithSubgraph(subgraph Subgraph) Graph {
	if subgraph.ID == "" {
		return g.Clone()
	}
	cloned := g.Clone()
	for i, existing := range cloned.Subgraphs {
		if existing.ID == subgraph.ID {
			cloned.Subgraphs[i] = subgraph
			cloned.Revision++
			return cloned
		}
	}
	cloned.Subgraphs = append(cloned.Subgraphs, subgraph)
	cloned.Revision++
	return cloned
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// parseRef 从 FromRef 拆出 id；前缀不匹配或 id 为空则失败。
func parseRef(ref, prefix string) (string, bool) {
	id, ok := strings.CutPrefix(ref, prefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// nodeByID 返回图中第一个匹配 ID 的节点。重复 ID 以先出现的为准。
func (g Graph) nodeByID(id string) (Node, bool) {
	if id == "" {
		return Node{}, false
	}
	// ponytail: 线性扫描，图变大后再建索引
	for _, node := range g.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

// nodeInSubgraphs 判断节点是否属于 wanted 中的任一子图。
func nodeInSubgraphs(node Node, wanted map[string]struct{}) bool {
	for _, id := range node.SubgraphIDs {
		if _, ok := wanted[id]; ok {
			return true
		}
	}
	return false
}

// cloneNode 拷贝节点及其切片字段，避免调用方改到图内数据。
func cloneNode(node Node) Node {
	cloned := node
	cloned.SubgraphIDs = append([]string(nil), node.SubgraphIDs...)
	cloned.SourceRefs = append([]string(nil), node.SourceRefs...)
	return cloned
}
