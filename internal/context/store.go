package context

import (
	"fmt"
	"sync"
)

// Store 按环境 ID 保存记忆图快照。Spawn 用 Fork 复制父快照并记下合入基线；之后各环境独立写入。
type Store struct {
	mu        sync.Mutex
	graphs    map[string]Graph
	baselines map[string]Graph // childID → Fork 瞬间的父快照
}

// NewStore 返回空的按环境隔离存储。
func NewStore() *Store {
	return &Store{
		graphs:    make(map[string]Graph),
		baselines: make(map[string]Graph),
	}
}

// Load 返回该环境的图拷贝；不存在时返回空图。
func (s *Store) Load(envID string) Graph {
	s.mu.Lock()
	defer s.mu.Unlock()
	graph, ok := s.graphs[envID]
	if !ok {
		return Graph{}
	}
	return graph.Clone()
}

// Save 用拷贝替换该环境的图快照。
func (s *Store) Save(envID string, graph Graph) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	s.graphs[envID] = graph.Clone()
}

// View 返回该环境的记忆图视图。Snapshot 读 Load，Commit 写 Save。
func (s *Store) View(envID string) *EnvView {
	return &EnvView{store: s, envID: envID}
}

// EnvView 是某个环境在 Store 上的记忆图视图。
type EnvView struct {
	store *Store
	envID string
}

// Snapshot 返回该环境的图拷贝。
func (v *EnvView) Snapshot() Graph {
	return v.store.Load(v.envID)
}

// Commit 用拷贝替换该环境的图快照。
func (v *EnvView) Commit(graph Graph) {
	v.store.Save(v.envID, graph)
}

// Fork 把父环境快照复制到子环境，并记下当时的父快照作为合入基线。
// 子环境已存在时不覆盖，也不改基线。
func (s *Store) Fork(parentID, childID string) {
	if childID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	if _, exists := s.graphs[childID]; exists {
		return
	}
	parent := Graph{}
	if parentID != "" {
		parent = s.graphs[parentID].Clone()
	}
	s.graphs[childID] = parent.Clone()
	if s.baselines == nil {
		s.baselines = make(map[string]Graph)
	}
	s.baselines[childID] = parent.Clone()
}

// Merge 把 from 相对其 Fork 基线的增量并入 into。纯加法：同 ID 同陈述跳过；
// 同 ID 不同陈述则保留 into、给 from 的节点换新 ID 并重写边引用。缺图当空图。
func (s *Store) Merge(from, into string) error {
	if into == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	var base Graph
	if s.baselines != nil {
		base = s.baselines[from].Clone()
	}
	ours := s.graphs[into].Clone()
	theirs := s.graphs[from].Clone()
	s.graphs[into] = mergeAdditive(from, base, ours, theirs)
	return nil
}

// mergeAdditive 以 ours 为底，并入 theirs 相对 base 的节点/子图/边，Revision 加一。
func mergeAdditive(fromID string, base, ours, theirs Graph) Graph {
	result := ours.Clone()
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

		if baseNode, ok := base.nodeByID(node.ID); ok && baseNode.Statement == node.Statement {
			continue
		}
		if oursNode, ok := result.nodeByID(node.ID); ok {
			if oursNode.Statement == node.Statement {
				continue
			}
			if id := nodeIDWithStatement(result, node.Statement); id != "" {
				remap[node.ID] = id
				continue
			}
			newID := unusedNodeID(fromID, node.ID, used)
			cloned := cloneNode(node)
			cloned.ID = newID
			result.Nodes = append(result.Nodes, cloned)
			used[newID] = struct{}{}
			remap[node.ID] = newID
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
		edge = rewriteEdge(edge, remap)
		if hasEdge(result, edge) {
			continue
		}
		result.Edges = append(result.Edges, edge)
	}

	result.Revision = ours.Revision + 1
	return result
}

func nodeIDWithStatement(g Graph, statement string) string {
	for _, node := range g.Nodes {
		if node.ID != "" && node.Statement == statement {
			return node.ID
		}
	}
	return ""
}

func unusedNodeID(fromID, oldID string, used map[string]struct{}) string {
	id := fromID + "-" + oldID
	if id != oldID {
		if _, exists := used[id]; !exists {
			return id
		}
	}
	for i := 1; ; i++ {
		id = fmt.Sprintf("mem-%d", i)
		if _, exists := used[id]; !exists {
			return id
		}
	}
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
