package coordination

import (
	"fmt"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// Stores 是一次 Run 共用的领域存储。Files / Exec 可以为空。
type Stores struct {
	Memory *ctxgraph.Store
	Files  *vfs.Store        // 可以为空
	Exec   *tmexec.Scheduler // 可以为空
}

// ProjectManagerUserMessage 把用户消息投影到 manager 固定子图。
func (s Stores) ProjectManagerUserMessage(text string) error {
	return s.projectManagerNode(ctxgraph.Node{
		Kind:           ctxgraph.NodeKindDirective,
		Statement:      "[User Message] " + text,
		SourceRefs:     []string{"user"},
		CreatorAgentID: "user",
	})
}

// ProjectManagerTaskInfos 把 task info 原子投影到 manager 固定子图。
func (s Stores) ProjectManagerTaskInfos(tasks []Task) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	nodes := make([]ctxgraph.Node, 0, len(tasks))
	for _, task := range tasks {
		if task.Info == "" {
			continue
		}
		nodes = append(nodes, ctxgraph.Node{
			ID:             "task-info-" + task.ID,
			Kind:           ctxgraph.NodeKindDirective,
			Statement:      "[Task Info] " + task.ID + ": " + task.Info,
			Status:         ctxgraph.NodeStatusAccepted,
			SourceRefs:     []string{"task:" + task.ID},
			CreatorAgentID: "system",
		})
	}
	return s.Memory.AppendNodes(ManagerEnvID, ManagerMemorySubgraph(), nodes)
}

// ProjectManagerTaskReport 把 task 报告投影到 manager 固定子图。
func (s Stores) ProjectManagerTaskReport(task Task, statement string) error {
	return s.projectManagerNode(taskReportNode(task, "task-report-"+task.ID, statement))
}

// ProjectJoinedTaskReport 把 join 报告同时投影到父 task 启动包和 manager 固定子图。
func (s Stores) ProjectJoinedTaskReport(parent, child Task, output string) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	statement := fmt.Sprintf("[Task Report] %s:\n%s", child.ID, output)
	if err := s.Memory.AppendNode(
		parent.Env.ID,
		TaskPackageSubgraph(parent.ID),
		taskReportNode(child, "joined-report-"+child.ID, statement),
	); err != nil {
		return err
	}
	return s.ProjectManagerTaskReport(child, statement)
}

func (s Stores) projectManagerNode(node ctxgraph.Node) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	node.Status = ctxgraph.NodeStatusAccepted
	return s.Memory.AppendNode(ManagerEnvID, ManagerMemorySubgraph(), node)
}

func taskReportNode(task Task, id, statement string) ctxgraph.Node {
	return ctxgraph.Node{
		ID:             id,
		Kind:           ctxgraph.NodeKindFact,
		Statement:      statement,
		SourceRefs:     []string{"task:" + task.ID},
		CreatorAgentID: task.Verifier.ID,
	}
}

// Fork 把父环境复制到子环境；Files 为空时只 fork 记忆。
func (s Stores) Fork(parentID, childID string) error {
	if s.Memory != nil {
		if err := s.Memory.Fork(parentID, childID); err != nil {
			return err
		}
	}
	if s.Files != nil {
		if err := s.Files.Fork(parentID, childID); err != nil {
			return err
		}
	}
	return nil
}

// Merge 把 from 环境合入 into。
func (s Stores) Merge(from, into string) error {
	return s.MergeInto(from, into, into)
}

// MergeInto 把文件和记忆增量分别合入各自的目标环境。
func (s Stores) MergeInto(from, memoryInto, filesInto string) error {
	if s.Files != nil {
		if err := s.Files.Merge(from, filesInto); err != nil {
			return err
		}
	}
	if s.Memory != nil {
		if err := s.Memory.Merge(from, memoryInto); err != nil {
			return err
		}
	}
	return nil
}

// DiscardFiles 删除一次性文件环境及其仍在运行的命令。
func (s Stores) DiscardFiles(envID string) error {
	if s.Exec != nil {
		s.Exec.Reap(envID)
	}
	if s.Files == nil {
		return nil
	}
	return s.Files.Discard(envID)
}
