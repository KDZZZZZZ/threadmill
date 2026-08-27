package context

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type storeState struct {
	Graphs    map[string]Graph `json:"graphs"`
	Baselines map[string]Graph `json:"baselines"`
}

// Store 按环境 ID 保存记忆图快照。Spawn 用 Fork 复制父快照并记下合入基线；之后各环境独立写入。
type Store struct {
	mu        sync.Mutex
	graphs    map[string]Graph
	baselines map[string]Graph // childID → Fork 瞬间的父快照
	path      string
}

// StoreStats 汇总内存图存储的规模。数量按环境快照求和。
type StoreStats struct {
	Environments int `json:"environments"`
	Baselines    int `json:"baselines"`
	Subgraphs    int `json:"subgraphs"`
	Nodes        int `json:"nodes"`
	Edges        int `json:"edges"`
}

// NewStore 返回空的按环境隔离存储。
func NewStore() *Store {
	return &Store{
		graphs:    make(map[string]Graph),
		baselines: make(map[string]Graph),
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
		Baselines:    len(s.baselines),
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
	store.baselines = cloneGraphMap(state.Baselines)
	return store, nil
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

// Fork 把父环境快照复制到子环境，并记下当时的父快照作为合入基线。
// 子环境已存在时不覆盖，也不改基线。
func (s *Store) Fork(parentID, childID string) error {
	if childID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.graphs == nil {
		s.graphs = make(map[string]Graph)
	}
	if _, exists := s.graphs[childID]; exists {
		return nil
	}
	parent := Graph{}
	if parentID != "" {
		parent = s.graphs[parentID].Clone()
	}
	s.graphs[childID] = parent.Clone()
	if s.baselines == nil {
		s.baselines = make(map[string]Graph)
	}
	previousBaseline, hadBaseline := s.baselines[childID]
	s.baselines[childID] = parent.Clone()
	if err := s.persistLocked(); err != nil {
		delete(s.graphs, childID)
		if hadBaseline {
			s.baselines[childID] = previousBaseline
		} else {
			delete(s.baselines, childID)
		}
		return err
	}
	return nil
}

// Merge 把 from 相对其 Fork 基线的增量并入 into。同 ID 同陈述则并集 SubgraphIDs
// （加入 A 与加入 B 互不影响）；同 ID 不同陈述则保留 into、给 from 换新 ID 并重写边。
// 缺图当空图。
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
	return s.commitGraphLocked(into, ours.mergeAdditive(from, base, theirs))
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(storeState{
		Graphs:    s.graphs,
		Baselines: s.baselines,
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
