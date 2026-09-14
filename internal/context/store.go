package context

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type storeState struct {
	Graphs    map[string]Graph `json:"graphs"`
	Snapshots map[string]bool  `json:"snapshots,omitempty"`
}

// Store 保存按环境隔离的记忆图和不可变输入、出口快照。
type Store struct {
	mu        sync.Mutex
	graphs    map[string]Graph
	snapshots map[string]bool // immutable output and ready-state references
	path      string
}

// StoreStats 汇总内存图存储的规模。数量按环境快照求和。
type StoreStats struct {
	Environments int `json:"environments"`
	Subgraphs    int `json:"subgraphs"`
	Nodes        int `json:"nodes"`
	Edges        int `json:"edges"`
}

// NewStore 返回空的按环境隔离存储。
func NewStore() *Store {
	return &Store{
		graphs:    make(map[string]Graph),
		snapshots: make(map[string]bool),
	}
}

// Stats 返回全部环境图的并发一致规模快照。
func (s *Store) Stats() StoreStats {
	if s == nil {
		return StoreStats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := StoreStats{
		Environments: len(s.graphs),
	}
	for _, graph := range s.graphs {
		stats.Subgraphs += len(graph.Subgraphs)
		stats.Nodes += len(graph.Nodes)
		stats.Edges += len(graph.Edges)
	}
	return stats
}

// OpenStore 打开持久化记忆图存储；文件不存在时创建空存储。
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("context: store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create memory store directory: %w", err)
	}
	store := NewStore()
	store.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read memory store %q: %w", path, err)
		}
		if err := store.persistLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}
	var state storeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode memory store %q: %w", path, err)
	}
	store.graphs = cloneGraphMap(state.Graphs)
	for ref, immutable := range state.Snapshots {
		if !immutable {
			continue
		}
		if _, exists := store.graphs[ref]; !exists {
			return nil, fmt.Errorf("context: snapshot %q has no graph", ref)
		}
		store.snapshots[ref] = true
	}
	return store, nil
}

// Load 返回该环境的图拷贝；不存在时返回空图。
func (s *Store) Load(envID string) Graph {
	graph, _ := s.Snapshot(envID)
	return graph
}

// Snapshot returns an isolated graph copy and distinguishes missing state from
// a valid empty graph. IDs may name a live environment or an immutable snapshot.
func (s *Store) Snapshot(id string) (Graph, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	graph, ok := s.graphs[id]
	if !ok {
		return Graph{}, false
	}
	return graph.Clone(), true
}

// SaveSnapshot seals a graph under an immutable reference. Replaying the same
// graph is idempotent; an existing reference cannot acquire different contents.
func (s *Store) SaveSnapshot(ref string, graph Graph) error {
	if ref == "" {
		return fmt.Errorf("context: snapshot reference is required")
	}
	if err := graph.ValidateReferences(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.graphs[ref]
	if existed && !sameGraph(previous, graph) {
		return fmt.Errorf("context: snapshot %q already has different contents", ref)
	}
	if s.snapshots[ref] {
		return nil
	}
	if s.snapshots == nil {
		s.snapshots = make(map[string]bool)
	}
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	s.graphs[ref], s.snapshots[ref] = graph.Clone(), true
	if err := s.persistLocked(); err != nil {
		delete(s.snapshots, ref)
		if existed {
			s.graphs[ref] = previous
		} else {
			delete(s.graphs, ref)
		}
		return err
	}
	return nil
}

// Restore binds the exact sealed graph to an environment, including managed
// memory. Only the runtime should use this seam when publishing a ready state pair;
// ordinary EnvView.Commit continues to preserve managed memory.
func (s *Store) Restore(envID, snapshotRef string) error {
	if envID == "" {
		return fmt.Errorf("context: target environment is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshots[snapshotRef] {
		return fmt.Errorf("context: unknown immutable snapshot %q", snapshotRef)
	}
	if s.snapshots[envID] && envID != snapshotRef {
		return fmt.Errorf("context: cannot restore over snapshot %q", envID)
	}
	return s.commitGraphLocked(envID, s.graphs[snapshotRef])
}

// Revision 返回该环境的图版本号，不拷贝图内容，供缓存层作失效提示。
func (s *Store) Revision(envID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if graph, ok := s.graphs[envID]; ok {
		return graph.Revision
	}
	return 0
}

// Save 用拷贝替换该环境的图快照。
func (s *Store) Save(envID string, graph Graph) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	graph = s.graphs[envID].preservingManaged(graph)
	return s.commitGraphLocked(envID, graph)
}

// EnsureSubgraph 保证环境中存在由运行时管理的子图。
func (s *Store) EnsureSubgraph(envID string, subgraph Subgraph) error {
	if envID == "" || subgraph.ID == "" {
		return fmt.Errorf("context: env and subgraph IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	graph := s.graphs[envID]
	if hasSubgraphID(graph, subgraph.ID) {
		return nil
	}
	return s.commitGraphLocked(envID, graph.WithSubgraph(subgraph))
}

// AppendNode 把节点原子提交到指定运行时子图；空 ID 分配新节点，显式 ID 更新同一节点。
func (s *Store) AppendNode(envID string, subgraph Subgraph, node Node) error {
	return s.AppendNodes(envID, subgraph, []Node{node})
}

// AppendNodes 把一组节点一次性提交到指定运行时子图。
func (s *Store) AppendNodes(envID string, subgraph Subgraph, nodes []Node) error {
	if envID == "" || subgraph.ID == "" {
		return fmt.Errorf("context: env, subgraph and statement are required")
	}
	if len(nodes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	graph := s.graphs[envID]
	for _, node := range nodes {
		if node.Statement == "" {
			return fmt.Errorf("context: env, subgraph and statement are required")
		}
		var err error
		graph, err = graph.withRuntimeNode(subgraph, node)
		if err != nil {
			return err
		}
	}
	return s.commitGraphLocked(envID, graph)
}

// DropSubgraph 删除子图及其全部节点；多重归属节点也删除，避免专属内容从别的归属泄漏。
func (s *Store) DropSubgraph(envID, subgraphID string) error {
	if envID == "" || subgraphID == "" {
		return fmt.Errorf("context: env and subgraph IDs are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitGraphLocked(envID, s.graphs[envID].withoutSubgraph(subgraphID))
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

// Revision 返回该环境当前图的版本号，不产生拷贝。
func (v *EnvView) Revision() int64 {
	return v.store.Revision(v.envID)
}

// Commit 用拷贝替换该环境的图快照。
func (v *EnvView) Commit(graph Graph) error {
	return v.store.Save(v.envID, graph)
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(storeState{
		Graphs:    s.graphs,
		Snapshots: s.snapshots,
	})
	if err != nil {
		return fmt.Errorf("encode memory store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write memory store %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit memory store %q: %w", s.path, err)
	}
	return nil
}

func (s *Store) commitGraphLocked(envID string, graph Graph) error {
	if s.snapshots[envID] {
		if !sameGraph(s.graphs[envID], graph) {
			return fmt.Errorf("context: snapshot %q is immutable", envID)
		}
		return nil
	}
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	previous, existed := s.graphs[envID]
	s.graphs[envID] = graph.Clone()
	if err := s.persistLocked(); err != nil {
		if existed {
			s.graphs[envID] = previous
		} else {
			delete(s.graphs, envID)
		}
		return err
	}
	return nil
}

func cloneGraphMap(src map[string]Graph) map[string]Graph {
	dst := make(map[string]Graph, len(src))
	for id, graph := range src {
		dst[id] = graph.Clone()
	}
	return dst
}

func sameGraph(a, b Graph) bool {
	return a.Revision == b.Revision && slices.Equal(a.Subgraphs, b.Subgraphs) &&
		slices.Equal(a.Edges, b.Edges) && slices.EqualFunc(a.Nodes, b.Nodes, sameNode)
}
