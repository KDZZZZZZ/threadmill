package context

import "sync"

// Store 按环境 ID 保存记忆图快照。Spawn 用 Fork 复制父快照；之后各环境独立写入。
type Store struct {
	mu     sync.Mutex
	graphs map[string]Graph
}

// NewStore 返回空的按环境隔离存储。
func NewStore() *Store {
	return &Store{graphs: make(map[string]Graph)}
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

// Fork 把父环境快照复制到子环境。子环境已存在时不覆盖。
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
	if parentID == "" {
		s.graphs[childID] = Graph{}
		return
	}
	s.graphs[childID] = s.graphs[parentID].Clone()
}
