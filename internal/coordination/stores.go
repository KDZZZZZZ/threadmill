package coordination

import (
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// Stores 是一次 Run 共用的领域存储。Files 可以为空。
type Stores struct {
	Memory *ctxgraph.Store
	Files  *vfs.Store // 可以为空
}

// Fork 把父环境复制到子环境；Files 为空时只 fork 记忆。
func (s Stores) Fork(parentID, childID string) {
	if s.Memory != nil {
		s.Memory.Fork(parentID, childID)
	}
	if s.Files != nil {
		s.Files.Fork(parentID, childID)
	}
}

// Merge 把 from 环境合入 into。
func (s Stores) Merge(from, into string) error {
	if s.Files != nil {
		if err := s.Files.Merge(from, into); err != nil {
			return err
		}
	}
	if s.Memory != nil {
		if err := s.Memory.Merge(from, into); err != nil {
			return err
		}
	}
	return nil
}
