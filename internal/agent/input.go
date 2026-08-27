package agent

import (
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Block 是插在消息历史之后的一段状态文本（记忆投影、协调图投影等 C3 块）。
type Block struct {
	ID   string // 例如 "memory"、"coordination"
	Text string
}

// Request 是一次模型生成所需的静态 Agent 配置和动态上下文快照。
// wire 顺序是 静态前缀（C0 角色提示 + tools）→ append-only 历史（Messages）
// → 易变状态（StateBlocks）→ 当轮尾部（Suffix）；把易变块移到历史之后，
// 其变动只会作废尾部缓存，不再打掉整段前缀。
type Request struct {
	SystemPrompt string
	Messages     []Message
	StateBlocks  []Block
	Suffix       string
	Tools        []agenttool.Definition
	// CacheKey 进入 Responses 的 prompt_cache_key，让同一 Agent 的请求粘在同一个缓存路由上。
	CacheKey string
}

// SetBlock 按 ID 覆盖或追加状态块，使 hook 顺序无关且幂等。
func (r *Request) SetBlock(id, text string) {
	for i := range r.StateBlocks {
		if r.StateBlocks[i].ID == id {
			r.StateBlocks[i].Text = text
			return
		}
	}
	r.StateBlocks = append(r.StateBlocks, Block{ID: id, Text: text})
}

// WirePrompt 拼接角色提示、全部状态块和尾段，供只读消费者（角色识别、token 估算）使用。
func (r Request) WirePrompt() string {
	parts := make([]string, 0, len(r.StateBlocks)+2)
	if r.SystemPrompt != "" {
		parts = append(parts, r.SystemPrompt)
	}
	for _, block := range r.StateBlocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	if r.Suffix != "" {
		parts = append(parts, r.Suffix)
	}
	return strings.Join(parts, "\n\n")
}

// cloneRequest 深拷贝模型请求中的消息、调用参数和工具 schema。
func cloneRequest(request Request) Request {
	definitions := make([]agenttool.Definition, len(request.Tools))
	for i, definition := range request.Tools {
		definitions[i] = cloneDefinition(definition)
	}
	blocks := make([]Block, len(request.StateBlocks))
	copy(blocks, request.StateBlocks)
	return Request{
		SystemPrompt: request.SystemPrompt,
		Messages:     cloneMessages(request.Messages),
		StateBlocks:  blocks,
		Suffix:       request.Suffix,
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
