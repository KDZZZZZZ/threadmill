package agent

import (
	"bytes"
	"encoding/json"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Role 标识一条对话消息的来源。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// UserMessage 是进入 ReAct 循环的一条用户消息。
type UserMessage struct {
	Content string
}

// AssistantMessage 是模型生成的一步结果；存在 ToolCalls 时循环继续执行工具。
type AssistantMessage struct {
	Content   string
	ToolCalls []agenttool.Call
	// ModelData 保存 Provider 下一轮无损续接所需的不透明 JSON 状态。
	ModelData json.RawMessage
	// Usage 保存本次 Provider 请求返回的 Token 用量。
	Usage *Usage
}

// Message 是模型下一次生成时可见的一条历史消息。
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []agenttool.Call
	ToolResult *agenttool.Result
	// ModelData 只由生成它的 Provider 解释。
	ModelData json.RawMessage
	// Usage 记录该助手消息对应的单次 Provider 用量。
	Usage *Usage
}

// cloneAssistantMessage 深拷贝助手消息中的工具调用。
func cloneAssistantMessage(message AssistantMessage) AssistantMessage {
	message.ToolCalls = cloneCalls(message.ToolCalls)
	message.ModelData = bytes.Clone(message.ModelData)
	message.Usage = cloneUsage(message.Usage)
	return message
}

// cloneMessages 深拷贝对话消息切片。
func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for i, message := range messages {
		cloned[i] = cloneMessage(message)
	}
	return cloned
}

// cloneMessage 深拷贝单条对话消息。
func cloneMessage(message Message) Message {
	cloned := message
	cloned.ToolCalls = cloneCalls(message.ToolCalls)
	cloned.ModelData = bytes.Clone(message.ModelData)
	cloned.Usage = cloneUsage(message.Usage)
	if message.ToolResult != nil {
		result := *message.ToolResult
		cloned.ToolResult = &result
	}
	return cloned
}

// messageFromAssistant 将 Provider 响应转换为历史消息并隔离可变数据。
func messageFromAssistant(response AssistantMessage) Message {
	return Message{
		Role:      RoleAssistant,
		Content:   response.Content,
		ToolCalls: cloneCalls(response.ToolCalls),
		ModelData: bytes.Clone(response.ModelData),
		Usage:     cloneUsage(response.Usage),
	}
}

// Messages 返回当前对话记录的深拷贝。
func (l *Loop) Messages() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneMessages(l.messages)
}

// appendToolResult 将工具结果转换成模型可见的工具消息。
func (l *Loop) appendToolResult(result agenttool.Result) {
	copyOfResult := result
	l.appendMessage(Message{
		Role:       RoleTool,
		Content:    result.Content,
		ToolResult: &copyOfResult,
	})
}

// appendMessage 将一条消息的深拷贝追加到对话记录。
func (l *Loop) appendMessage(message Message) {
	l.mu.Lock()
	l.messages = append(l.messages, cloneMessage(message))
	l.mu.Unlock()
}
