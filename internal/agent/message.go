package agent

import (
	"bytes"
	"encoding/json"
	"time"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Role 标识一条对话消息的来源。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// StopReason 是助手消息结束的原因，取值对齐 Pi 的 stopReason。
// 来源：https://github.com/badlogic/pi-mono/blob/main/packages/ai/src/types.ts
type StopReason string

const (
	StopReasonStop    StopReason = "stop"
	StopReasonLength  StopReason = "length"
	StopReasonToolUse StopReason = "toolUse"
	StopReasonError   StopReason = "error"
	StopReasonAborted StopReason = "aborted"
)

// UserMessage 是进入 ReAct 循环的一条用户消息。
type UserMessage struct {
	Content   string
	Timestamp int64
}

// AssistantMessage 是模型生成的一步结果；存在 ToolCalls 时循环继续执行工具。
type AssistantMessage struct {
	Content      string
	Thinking     string
	ToolCalls    []agenttool.Call
	ModelData    json.RawMessage
	Usage        *Usage
	Timestamp    int64
	StopReason   StopReason
	ErrorMessage string
	API          string
	Provider     string
	Model        string
}

// Message 是模型下一次生成时可见的一条历史消息。
type Message struct {
	Role         Role
	Content      string
	Thinking     string
	ToolCalls    []agenttool.Call
	ToolResult   *agenttool.Result
	ModelData    json.RawMessage
	Usage        *Usage
	Timestamp    int64
	StopReason   StopReason
	ErrorMessage string
	API          string
	Provider     string
	Model        string
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
		result.Details = bytes.Clone(message.ToolResult.Details)
		cloned.ToolResult = &result
	}
	return cloned
}

// messageFromAssistant 将 Provider 响应转换为历史消息并隔离可变数据。
func messageFromAssistant(response AssistantMessage) Message {
	response = stampAssistant(response)
	return Message{
		Role:         RoleAssistant,
		Content:      response.Content,
		Thinking:     response.Thinking,
		ToolCalls:    cloneCalls(response.ToolCalls),
		ModelData:    bytes.Clone(response.ModelData),
		Usage:        cloneUsage(response.Usage),
		Timestamp:    response.Timestamp,
		StopReason:   response.StopReason,
		ErrorMessage: response.ErrorMessage,
		API:          response.API,
		Provider:     response.Provider,
		Model:        response.Model,
	}
}

func stampAssistant(message AssistantMessage) AssistantMessage {
	if message.Timestamp == 0 {
		message.Timestamp = timestampMillis()
	}
	if message.StopReason == "" {
		if len(message.ToolCalls) > 0 {
			message.StopReason = StopReasonToolUse
		} else {
			message.StopReason = StopReasonStop
		}
	}
	return message
}

func timestampMillis() int64 {
	return time.Now().UnixMilli()
}

func lastAssistantText(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}

// Messages 返回当前对话记录的深拷贝。
func (l *Loop) Messages() []Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneMessages(l.messages)
}

// appendToolResult 将工具结果转换成模型可见的工具消息。
func (l *Loop) appendToolResult(result agenttool.Result) error {
	copyOfResult := result
	copyOfResult.Details = bytes.Clone(result.Details)
	return l.appendMessage(Message{
		Role:       RoleTool,
		Content:    result.Content,
		ToolResult: &copyOfResult,
		Timestamp:  timestampMillis(),
	})
}

// appendMessage 将一条消息的深拷贝追加到对话记录，并刷新进行中的 ReAct 快照。
func (l *Loop) appendMessage(message Message) error {
	l.mu.Lock()
	if message.Timestamp == 0 {
		message.Timestamp = timestampMillis()
	}
	l.messages = append(l.messages, cloneMessage(message))
	l.mu.Unlock()
	return l.persistReact()
}
