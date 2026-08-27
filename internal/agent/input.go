package agent

import (
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Request 是一次模型生成所需的静态 Agent 配置和动态上下文快照。
type Request struct {
	SystemPrompt string
	Messages     []Message
	Tools        []agenttool.Definition
	// CacheKey 进入 Responses 的 prompt_cache_key，让同一 Agent 的请求粘在同一个缓存路由上。
	CacheKey string
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
		CacheKey:     request.CacheKey,
	}
}

// SetSubscribedSubgraphs 替换当前生效的子图订阅列表。
func (l *Loop) SetSubscribedSubgraphs(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subscribedSubgraphs = append([]string(nil), ids...)
}

// SetFixedSubscribedSubgraphs 设置运行时固定订阅；普通订阅和 checkpoint 不会覆盖它。
func (l *Loop) SetFixedSubscribedSubgraphs(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fixedSubscribedSubgraphs = uniqueIDs(ids)
}

func (l *Loop) subscribeSubgraph(id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.subscribedSubgraphs {
		if existing == id {
			return
		}
	}
	l.subscribedSubgraphs = append(l.subscribedSubgraphs, id)
}

// assembleRequest 将静态 Agent 配置、当前消息和工具定义组装成请求快照。
func (l *Loop) assembleRequest() Request {
	l.mu.Lock()
	defer l.mu.Unlock()

	definitions := make([]agenttool.Definition, 0, len(l.definitions))
	for _, definition := range l.definitions {
		if tool, ok := l.tools[definition.Name]; ok && toolHidden(tool) {
			continue
		}
		definitions = append(definitions, cloneDefinition(definition))
	}
	return Request{
		SystemPrompt: l.agentConfig.systemPrompt,
		Messages:     cloneMessages(l.messages),
		Tools:        definitions,
		CacheKey:     l.agentID,
	}
}

func assembleSystemPrompt(systemPrompt string, graph ctxgraph.Graph, subscribed []string) string {
	return joinSystemPrompt(systemPrompt, formatMemory(graph.NodesInSubgraphs(subscribed)))
}

func joinSystemPrompt(systemPrompt, extra string) string {
	if extra == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return extra
	}
	return systemPrompt + "\n\n" + extra
}

func formatMemory(nodes []ctxgraph.Node) string {
	var b strings.Builder
	for _, node := range nodes {
		if node.Statement == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("记忆：")
		}
		b.WriteString("\n- ")
		if node.Kind != "" || node.Status != "" {
			fmt.Fprintf(&b, "[%s/%s] ", node.Kind, node.Status)
		}
		b.WriteString(node.Statement)
	}
	return b.String()
}
