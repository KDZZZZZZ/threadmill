package agent

import agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"

// Request 是一次模型生成所需的静态 Agent 配置和动态上下文快照。
type Request struct {
	SystemPrompt string
	Messages     []Message
	Tools        []agenttool.Definition
}

// cloneRequest 深拷贝模型请求中的消息、调用参数和工具 schema。
func cloneRequest(request Request) Request {
	definitions := make([]agenttool.Definition, len(request.Tools))
	for i, definition := range request.Tools {
		definitions[i] = cloneDefinition(definition)
	}
	return Request{
		SystemPrompt: request.SystemPrompt,
		Messages:     cloneMessages(request.Messages),
		Tools:        definitions,
	}
}

// assembleRequest 将静态 Agent 配置与当前动态消息、工具定义组装成请求快照。
func (l *Loop) assembleRequest() Request {
	l.mu.Lock()
	defer l.mu.Unlock()

	definitions := make([]agenttool.Definition, len(l.definitions))
	for i, definition := range l.definitions {
		definitions[i] = cloneDefinition(definition)
	}
	return Request{
		SystemPrompt: l.agentConfig.systemPrompt,
		Messages:     cloneMessages(l.messages),
		Tools:        definitions,
	}
}
