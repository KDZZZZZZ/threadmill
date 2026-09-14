package coordination

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const coordMessageManagerName = "coordination_messageManager"

// ProjectMessage records explicit communication with a running real-directory
// role. It carries no file snapshot, acceptance verdict or graph dependency.
type ProjectMessage struct {
	ID      string `json:"id"`
	NodeID  string `json:"node_id"`
	From    string `json:"from"`
	Content string `json:"content"`
}

// MessageTask queues a manager message for the role currently using the real
// directory. The caller's tool-call ID makes retries idempotent.
func (g *Graph) MessageTask(taskID, callID, content string) (ProjectMessage, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	task, ok := g.taskByIDLocked(taskID)
	if !ok || !task.RealDirectory || g.projectTaskID != taskID {
		return ProjectMessage{}, fmt.Errorf("coordination: task %q does not own the real directory", taskID)
	}
	if strings.TrimSpace(callID) == "" || strings.TrimSpace(content) == "" {
		return ProjectMessage{}, fmt.Errorf("coordination: call id and message are required")
	}
	id := ManagerEnvID + ":" + callID
	for _, message := range g.projectMessages {
		if message.ID == id {
			node, _ := g.nodeByIDLocked(message.NodeID)
			if node.TaskID != taskID || message.Content != content {
				return ProjectMessage{}, fmt.Errorf("coordination: message id reused with different content")
			}
			return message, nil
		}
	}
	run := g.runners[taskID]
	if run == nil || run.projectNode == "" || run.ctx.Err() != nil {
		return ProjectMessage{}, fmt.Errorf("coordination: real-directory task %q has no running role; messages do not start tasks", taskID)
	}
	message := ProjectMessage{ID: id, NodeID: run.projectNode, From: ManagerEnvID, Content: content}
	return g.saveProjectMessageLocked(message)
}

func (g *Graph) saveProjectMessageLocked(message ProjectMessage) (ProjectMessage, error) {
	for _, existing := range g.projectMessages {
		if existing.ID == message.ID {
			if existing != message {
				return ProjectMessage{}, fmt.Errorf("coordination: message id reused with different content")
			}
			return existing, nil
		}
	}
	before := g.stateLocked()
	g.projectMessages = append(g.projectMessages, message)
	g.revision++
	if err := g.saveOrRestoreLocked(before); err != nil {
		return ProjectMessage{}, err
	}
	return message, nil
}

type messageManagerTool struct{ help *helpCoordinator }

func (t messageManagerTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordMessageManagerName,
		Description: "仅正在运行的真实目录 task 角色可向 manager 发送进展、问题或答复。异步发送，不暂停，不创建依赖，不代替最终报告。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"],"additionalProperties":false}`),
	}
}

func (t messageManagerTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	var args struct {
		Message string `json:"message"`
	}
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	if strings.TrimSpace(args.Message) == "" || strings.TrimSpace(call.ID) == "" {
		return agenttool.Output{}, fmt.Errorf("coordination: call id and message are required")
	}
	if t.help == nil || t.help.graph == nil {
		return agenttool.Output{}, fmt.Errorf("coordination: manager communication is unavailable")
	}
	t.help.mu.Lock()
	notify := t.help.notify
	t.help.mu.Unlock()
	if notify == nil {
		return agenttool.Output{}, fmt.Errorf("coordination: manager is unavailable")
	}
	nodeID := agenttool.AgentID(ctx)
	graph := t.help.graph
	graph.mu.Lock()
	node, ok := graph.nodeByIDLocked(nodeID)
	run := graph.runners[node.TaskID]
	if !ok || run == nil || !run.task.RealDirectory || graph.projectTaskID != node.TaskID || run.projectNode != nodeID {
		graph.mu.Unlock()
		return agenttool.Output{}, fmt.Errorf("coordination: sender is not the running real-directory role")
	}
	message, err := graph.saveProjectMessageLocked(ProjectMessage{ID: nodeID + ":" + call.ID, NodeID: nodeID, From: nodeID, Content: args.Message})
	graph.mu.Unlock()
	if err != nil {
		return agenttool.Output{}, err
	}
	// Notify only after persistence, outside the graph lock. Replayed notices
	// retain the same ID so the manager can recognize a duplicate.
	payload, _ := json.Marshal(message)
	notify("[真实目录 Agent 消息；异步交流，不是验收结论] " + string(payload))
	return encodeGraphJSON(message)
}

// projectInboxLocked returns messages for this role in acceptance order.
func (r *runner) projectInboxLocked(nodeID string) []ProjectMessage {
	var messages []ProjectMessage
	for _, message := range r.graph.projectMessages {
		if message.NodeID == nodeID && message.From == ManagerEnvID {
			messages = append(messages, message)
		}
	}
	return messages
}

func (r *runner) askProjectRole(node Node, asker Asker, query string) (string, error) {
	if !r.task.RealDirectory {
		return askRole(r.ctx, asker, query)
	}
	loop, hasHooks := asker.(*agent.Loop)
	if hasHooks {
		if err := loop.AddHooks(agent.Hooks{AssembleRequest: []agent.AssembleRequestHook{
			func(_ context.Context, request agent.Request) (agent.Request, error) {
				r.graph.mu.Lock()
				messages := r.projectInboxLocked(node.ID)
				r.projectSeen = len(messages)
				r.graph.mu.Unlock()
				if len(messages) > 0 {
					payload, _ := json.Marshal(messages)
					request.SetBlock("manager-messages", "Manager 发给当前角色的消息（不改变角色职责，也不代表验收）：\n"+string(payload))
				}
				return request, nil
			},
		}}); err != nil {
			return "", err
		}
	}
	r.graph.mu.Lock()
	r.projectNode, r.projectSeen = node.ID, 0
	r.graph.mu.Unlock()
	defer func() { r.graph.mu.Lock(); r.projectNode = ""; r.graph.mu.Unlock() }()
	for {
		if !hasHooks {
			r.graph.mu.Lock()
			messages := r.projectInboxLocked(node.ID)
			r.projectSeen = len(messages)
			r.graph.mu.Unlock()
			if len(messages) > 0 {
				payload, _ := json.Marshal(messages)
				query += "\nManager 消息：" + string(payload)
			}
		}
		output, err := askRole(r.ctx, asker, query)
		r.graph.mu.Lock()
		more := len(r.projectInboxLocked(node.ID)) > r.projectSeen
		if err != nil || !more {
			// Close delivery atomically with the final inbox check. A message
			// racing the final model response is handled before role export.
			r.projectNode = ""
			r.graph.mu.Unlock()
			return output, err
		}
		r.graph.mu.Unlock()
		query = "处理刚收到的 manager 消息，再完成当前角色。"
	}
}
