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

func taskSnapshotEnvID(task Task) string {
	return task.Env.ID + ":completed"
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
	graph := s.Memory.Load(ManagerEnvID)
	sources := make([]ctxgraph.Node, 0, len(tasks))
	nodes := make([]ctxgraph.Node, 0, len(tasks))
	for _, task := range tasks {
		if task.Info == "" {
			continue
		}
		nodes = append(nodes, taskInfoNode(task))
		if task.SpawnedFrom != "" || hasNodeID(graph, taskUserInputNodeID(task.ID)) {
			continue
		}
		user, ok := userMessageForTask(graph, task.ID)
		if !ok {
			continue
		}
		sources = append(sources, ctxgraph.Node{
			ID:             taskUserInputNodeID(task.ID),
			Kind:           ctxgraph.NodeKindDirective,
			Statement:      user.Statement,
			Status:         ctxgraph.NodeStatusAccepted,
			SourceRefs:     append([]string(nil), user.SourceRefs...),
			CreatorAgentID: "system",
		})
	}
	if err := s.Memory.AppendNodes(ManagerEnvID, taskSourcesSubgraph(), sources); err != nil {
		return err
	}
	return s.Memory.AppendNodes(ManagerEnvID, ManagerMemorySubgraph(), nodes)
}

func taskUserInputNodeID(taskID string) string {
	return "task-user-input-" + taskID
}

func hasNodeID(graph ctxgraph.Graph, id string) bool {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return true
		}
	}
	return false
}

func userMessageForTask(graph ctxgraph.Graph, taskID string) (ctxgraph.Node, bool) {
	end := len(graph.Nodes)
	for i, node := range graph.Nodes {
		if node.ID == "task-info-"+taskID {
			end = i
			break
		}
	}
	for i := end - 1; i >= 0; i-- {
		if graph.Nodes[i].CreatorAgentID == "user" {
			return graph.Nodes[i], true
		}
	}
	return ctxgraph.Node{}, false
}

func taskInfoNode(task Task) ctxgraph.Node {
	return ctxgraph.Node{
		ID:             "task-info-" + task.ID,
		Kind:           ctxgraph.NodeKindDirective,
		Statement:      "[Task Info] " + task.ID + ": " + task.Info,
		Status:         ctxgraph.NodeStatusAccepted,
		SourceRefs:     []string{"task:" + task.ID},
		CreatorAgentID: "system",
	}
}

// ProjectManagerTaskReport 把 task 报告投影到 manager 固定子图。
// 无完整证据记录的 verdict 按 disputed 投影，避免自报结论以已成立事实进入记忆。
func (s Stores) ProjectManagerTaskReport(task Task, statement string) error {
	node := taskReportNode(task, "task-report-"+task.ID, statement)
	node.Status = reportNodeStatus(statement)
	return s.projectManagerNodeKeepingStatus(node)
}

// ProjectCandidateTaskReport keeps the manager informed without injecting a
// candidate's full output into the target role; the role reads it through join.
func (s Stores) ProjectCandidateTaskReport(child Task, output string) error {
	statement := fmt.Sprintf("[Task Report] %s:\n%s", child.ID, output)
	return s.ProjectManagerTaskReport(child, statement)
}

func (s Stores) projectManagerNodeKeepingStatus(node ctxgraph.Node) error {
	if s.Memory == nil {
		return ErrNilStore
	}
	return s.Memory.AppendNode(ManagerEnvID, ManagerMemorySubgraph(), node)
}

// reportNodeStatus 依据报告正文给出投影状态。运行时错误没有 verifier verdict，
// 可直接记录；模型 verdict 必须携带通用证据记录或兼容旧版命令证据。
func reportNodeStatus(output string) string {
	if reportVerdict(output) == "" || hasReportEvidence(output) {
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

func hasReportEvidence(output string) bool {
	return hasEvidenceRecord(output) || hasLegacyCommandEvidence(output)
}

func hasEvidenceRecord(output string) bool {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	for _, record := range strings.Split(output, "\n\n") {
		if hasReportField(record, "证据锚:", "证据锚：") &&
			hasReportField(record, "原始观察:", "原始观察：") &&
			hasReportField(record, "适用范围:", "适用范围：") {
			return true
		}
	}
	return false
}

func hasReportField(output string, prefixes ...string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		for _, prefix := range prefixes {
			value, ok := strings.CutPrefix(line, prefix)
			if ok && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func hasLegacyCommandEvidence(output string) bool {
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
