package coordination

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// TaskProgress 是尚未跑完的 task 游标：已完成角色的输出，以及已经合入过的节点。
type TaskProgress struct {
	Outputs map[string]string
	Merged  []string `json:",omitempty"`
}

// ProgressStore 保存、读取、删除进行中的 task 进度。
type ProgressStore interface {
	Save(taskID string, progress TaskProgress) error
	Load(taskID string) (TaskProgress, bool, error)
	Delete(taskID string) error
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write task progress %q: %w", tmp, err)
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
	return progress, true, nil
}

// Delete 扔掉已完成的 task 进度。
func (s *DirProgressStore) Delete(taskID string) error {
	err := os.Remove(s.path(taskID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete task progress: %w", err)
	}
	return nil
}
