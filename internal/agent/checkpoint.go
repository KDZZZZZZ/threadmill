package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Checkpoint 是一次尚未完成的 ReAct 快照；回合正常结束后会被扔掉。
type Checkpoint struct {
	Messages            []Message
	UsedToolCallIDs     []string
	SubscribedSubgraphs []string
	Committed           bool
}

// CheckpointStore 保存、读取、删除进行中的 ReAct。
type CheckpointStore interface {
	Save(agentID string, checkpoint Checkpoint) error
	Load(agentID string) (Checkpoint, bool, error)
	Delete(agentID string) error
}

// DirCheckpointStore 每个 Agent 一个 JSON 文件。
type DirCheckpointStore struct {
	dir string
}

var _ CheckpointStore = (*DirCheckpointStore)(nil)

// NewDirCheckpointStore 在 dir 下落盘进行中的 ReAct。
func NewDirCheckpointStore(dir string) (*DirCheckpointStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: checkpoint dir is required", ErrInvalidConfig)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint dir %q: %w", dir, err)
	}
	return &DirCheckpointStore{dir: dir}, nil
}

func (s *DirCheckpointStore) path(agentID string) string {
	return filepath.Join(s.dir, url.PathEscape(agentID)+".json")
}

// Save 覆盖该 Agent 当前进行中的 ReAct。
func (s *DirCheckpointStore) Save(agentID string, checkpoint Checkpoint) error {
	data, err := json.Marshal(checkpoint)
	if err != nil {
		return fmt.Errorf("encode react checkpoint: %w", err)
	}
	path := s.path(agentID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write react checkpoint %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit react checkpoint %q: %w", path, err)
	}
	return nil
}

// Load 读取进行中的 ReAct；没有文件时 ok 为 false。
func (s *DirCheckpointStore) Load(agentID string) (Checkpoint, bool, error) {
	data, err := os.ReadFile(s.path(agentID))
	if err != nil {
		if os.IsNotExist(err) {
			return Checkpoint{}, false, nil
		}
		return Checkpoint{}, false, fmt.Errorf("read react checkpoint: %w", err)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return Checkpoint{}, false, fmt.Errorf("decode react checkpoint: %w", err)
	}
	return checkpoint, true, nil
}

// Delete 扔掉已完成或已放弃的 ReAct 快照。
func (s *DirCheckpointStore) Delete(agentID string) error {
	err := os.Remove(s.path(agentID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete react checkpoint: %w", err)
	}
	return nil
}

func (l *Loop) persistReact() error {
	if l.checkpoints == nil {
		return nil
	}
	l.mu.Lock()
	checkpoint := l.snapshotReactLocked()
	agentID := l.agentID
	l.mu.Unlock()
	if err := l.checkpoints.Save(agentID, checkpoint); err != nil {
		return fmt.Errorf("saving react checkpoint: %w", err)
	}
	return nil
}

func (l *Loop) discardReact() error {
	if l.checkpoints == nil {
		return nil
	}
	if err := l.checkpoints.Delete(l.agentID); err != nil {
		return fmt.Errorf("discarding react checkpoint: %w", err)
	}
	return nil
}

func (l *Loop) markReactCommitted() error {
	l.mu.Lock()
	l.reactCommitted = true
	l.mu.Unlock()
	return l.persistReact()
}

func (l *Loop) committed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reactCommitted
}

func (l *Loop) restoreCheckpoint() (bool, error) {
	if l.checkpoints == nil {
		return false, nil
	}
	checkpoint, ok, err := l.checkpoints.Load(l.agentID)
	if err != nil || !ok {
		return false, err
	}
	l.mu.Lock()
	l.messages = cloneMessages(checkpoint.Messages)
	l.usedToolCallIDs = make(map[string]struct{}, len(checkpoint.UsedToolCallIDs))
	for _, id := range checkpoint.UsedToolCallIDs {
		l.usedToolCallIDs[id] = struct{}{}
	}
	l.subscribedSubgraphs = append([]string(nil), checkpoint.SubscribedSubgraphs...)
	l.reactCommitted = checkpoint.Committed
	l.mu.Unlock()
	return true, nil
}

func checkpointUser(messages []Message) UserMessage {
	for _, message := range messages {
		if message.Role == RoleUser {
			return UserMessage{Content: message.Content, Timestamp: message.Timestamp}
		}
	}
	return UserMessage{}
}

func reactComplete(messages []Message) bool {
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	return last.Role == RoleAssistant && len(last.ToolCalls) == 0
}

func unpairedToolCalls(messages []Message) []agenttool.Call {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != RoleAssistant || len(messages[i].ToolCalls) == 0 {
			continue
		}
		done := make(map[string]struct{})
		for _, message := range messages[i+1:] {
			if message.ToolResult != nil {
				done[message.ToolResult.CallID] = struct{}{}
			}
		}
		pending := make([]agenttool.Call, 0)
		for _, call := range messages[i].ToolCalls {
			if _, ok := done[call.ID]; !ok {
				pending = append(pending, cloneCall(call))
			}
		}
		return pending
	}
	return nil
}

func (l *Loop) snapshotReactLocked() Checkpoint {
	ids := make([]string, 0, len(l.usedToolCallIDs))
	for id := range l.usedToolCallIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return Checkpoint{
		Messages:            cloneMessages(l.messages),
		UsedToolCallIDs:     ids,
		SubscribedSubgraphs: append([]string(nil), l.subscribedSubgraphs...),
		Committed:           l.reactCommitted,
	}
}
