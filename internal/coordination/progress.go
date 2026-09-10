package coordination

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

const progressVersion = 1

// TaskProgress records one activation. Paired immutable role outputs live in Graph.
type TaskProgress struct {
	Version int             `json:"version"`
	Inputs  []InputProgress `json:"inputs,omitempty"`
	Pending *ExportProgress `json:"pending,omitempty"`
}

// ExportProgress journals a completed role until its paired output is visible.
// The graph body is temporary and is removed after the immutable reference commits.
type ExportProgress struct {
	Output Output         `json:"output"`
	Memory ctxgraph.Graph `json:"memory"`
}

// InputProgress is the durable state of a fixed incoming batch. Ready is published
// only when both referenced snapshots have been saved.
type InputProgress struct {
	ID        string                `json:"id"`
	NodeID    string                `json:"node_id"`
	ResumeFor string                `json:"resume_for,omitempty"`
	TargetID  string                `json:"target_id"`
	Sources   []InputSourceProgress `json:"sources"`
	Paths     []string              `json:"paths,omitempty"`
	Phase     string                `json:"phase"`
	FilesRef  string                `json:"files_ref,omitempty"`
	MemoryRef string                `json:"memory_ref,omitempty"`
	Reason    string                `json:"reason,omitempty"`
	Started   bool                  `json:"started,omitempty"`
}

// InputSourceProgress identifies an immutable source and its disposable file
// candidate. File decisions never change the source memory graph.
type InputSourceProgress struct {
	ID           string   `json:"id"`
	EnvID        string   `json:"env_id,omitempty"`
	FilesRef     string   `json:"files_ref,omitempty"`
	MemoryRef    string   `json:"memory_ref"`
	Output       string   `json:"output,omitempty"`
	Applied      bool     `json:"applied,omitempty"`
	AppliedAll   bool     `json:"applied_all,omitempty"`
	AppliedPaths []string `json:"applied_paths,omitempty"`
	Discarded    bool     `json:"discarded,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

// ProgressStore 保存、读取 task activation 的输入进度和待提交导出。
type ProgressStore interface {
	Save(taskID string, progress TaskProgress) error
	Load(taskID string) (TaskProgress, bool, error)
}

// DirProgressStore 每个 task 一个 JSON 文件。
type DirProgressStore struct {
	dir string
}

var _ ProgressStore = (*DirProgressStore)(nil)

// NewDirProgressStore 在 dir 下落盘进行中的 task 进度。
func NewDirProgressStore(dir string) (*DirProgressStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("coordination: progress dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create progress dir %q: %w", dir, err)
	}
	return &DirProgressStore{dir: dir}, nil
}

func (s *DirProgressStore) path(taskID string) string {
	return filepath.Join(s.dir, url.PathEscape(taskID)+".json")
}

// Save 覆盖该 task 当前进度。
func (s *DirProgressStore) Save(taskID string, progress TaskProgress) error {
	data, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("encode task progress: %w", err)
	}
	path := s.path(taskID)
	file, err := os.CreateTemp(s.dir, ".progress-*")
	if err != nil {
		return fmt.Errorf("create task progress: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit task progress %q: %w", path, err)
	}
	return nil
}

// Load 读取进行中的 task 进度；没有文件时 ok 为 false。
func (s *DirProgressStore) Load(taskID string) (TaskProgress, bool, error) {
	data, err := os.ReadFile(s.path(taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return TaskProgress{}, false, nil
		}
		return TaskProgress{}, false, fmt.Errorf("read task progress: %w", err)
	}
	var progress TaskProgress
	if err := json.Unmarshal(data, &progress); err != nil {
		return TaskProgress{}, false, fmt.Errorf("decode task progress: %w", err)
	}
	if progress.Version != progressVersion {
		return TaskProgress{}, false, fmt.Errorf("coordination: incompatible progress version %d; paired input checkpoints required", progress.Version)
	}
	return progress, true, nil
}

// memoryProgressStore provides the same retry semantics to in-process callers.
type memoryProgressStore struct {
	mu    sync.Mutex
	items map[string]TaskProgress
}

func (s *memoryProgressStore) Save(id string, progress TaskProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]TaskProgress)
	}
	s.items[id] = cloneTaskProgress(progress)
	return nil
}
func (s *memoryProgressStore) Load(id string) (TaskProgress, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress, ok := s.items[id]
	return cloneTaskProgress(progress), ok, nil
}
