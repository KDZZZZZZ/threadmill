package coordination

import (
	"errors"
	"fmt"
	"strings"

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
// 无命令证据的 PASS 按 disputed 投影，避免假阳性结论以已成立事实身份进入记忆。
func (s Stores) ProjectManagerTaskReport(task Task, statement string) error {
	node := taskReportNode(task, "task-report-"+task.ID, statement)
	node.Status = reportNodeStatus(statement)
	return s.projectManagerNodeKeepingStatus(node)
}

// ProjectJoinedTaskReport 把 join 报告同时投影到父 task 启动包和 manager 固定子图。
func (s Stores) ProjectJoinedTaskReport(parent, child Task, output string) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	statement := fmt.Sprintf("[Task Report] %s:\n%s", child.ID, output)
	node := taskReportNode(child, "joined-report-"+child.ID, statement)
	node.Status = reportNodeStatus(output)
	if err := s.Memory.AppendNode(
		parent.Env.ID,
		TaskPackageSubgraph(parent.ID),
		node,
	); err != nil {
		return err
	}
	return s.ProjectManagerTaskReport(child, statement)
}

func (s Stores) projectManagerNodeKeepingStatus(node ctxgraph.Node) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	return s.Memory.AppendNode(ManagerEnvID, ManagerMemorySubgraph(), node)
}

// reportNodeStatus 依据报告正文给出投影状态：FAIL/INCONCLUSIVE 或带命令证据的 PASS 记 accepted；
// 无命令证据的 PASS 记 disputed。命令证据按 verifier 输出格式约定（命令与退出码）识别。
func reportNodeStatus(output string) string {
	if reportVerdict(output) != "PASS" || hasCommandEvidence(output) {
		return ctxgraph.NodeStatusAccepted
	}
	return ctxgraph.NodeStatusDisputed
}

func reportVerdict(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"结论:", "结论："} {
			verdict, ok := strings.CutPrefix(trimmed, prefix)
			if !ok {
				continue
			}
			verdict = strings.TrimSpace(verdict)
			switch verdict {
			case "PASS", "FAIL", "INCONCLUSIVE":
				return verdict
			}
		}
	}
	return ""
}

func hasCommandEvidence(output string) bool {
	for _, marker := range []string{"退出码", "exit code", "exit_code", "exitCode", "Exit Code", "exit status"} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
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
	var err error
	if s.Exec != nil {
		err = s.Exec.Reap(envID)
	}
	if s.Files == nil {
		return err
	}
	return errors.Join(err, s.Files.Discard(envID))
}
