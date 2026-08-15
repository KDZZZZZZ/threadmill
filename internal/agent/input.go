package agent

import (
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

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

// SetContextGraph 写入本 Agent 的唯一图副本，并提交到全局记忆图。
func (l *Loop) SetContextGraph(graph ctxgraph.Graph) {
	l.mu.Lock()
	l.graphCopy.Graph = graph.Clone()
	owned := l.graphCopy
	l.mu.Unlock()
	ctxgraph.Update(owned)
}

// SetSubscribedSubgraphs 替换当前生效的子图订阅列表。
func (l *Loop) SetSubscribedSubgraphs(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subscribedSubgraphs = append([]string(nil), ids...)
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

// ContextGraph 返回本 Agent 当前持有的图副本快照。
func (l *Loop) ContextGraph() ctxgraph.Graph {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.graphCopy.Graph.Clone()
}

func (l *Loop) ownedCopy() ctxgraph.Copy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return ctxgraph.Copy{
		AgentID: l.graphCopy.AgentID,
		Graph:   l.graphCopy.Graph.Clone(),
	}
}

func (l *Loop) commitCopy(copy ctxgraph.Copy) {
	l.mu.Lock()
	l.graphCopy.Graph = copy.Graph.Clone()
	owned := l.graphCopy
	l.mu.Unlock()
	ctxgraph.Update(owned)
}

// assembleRequest 将静态 Agent 配置、当前消息和工具定义组装成请求快照。
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

func assembleSystemPrompt(systemPrompt string, graph ctxgraph.Graph, subscribed []string) string {
	memory := formatMemory(graph.NodesInSubgraphs(subscribed))
	if memory == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return memory
	}
	return systemPrompt + "\n\n" + memory
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
		b.WriteString(node.Statement)
	}
	return b.String()
}

func (l *Loop) refreshGraphCopyLocked() ctxgraph.Copy {
	l.graphCopy = ctxgraph.Clone(l.agentID)
	return l.graphCopy
}
