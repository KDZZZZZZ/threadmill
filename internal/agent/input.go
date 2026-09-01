package agent

import (
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Block 是按稳定性插入模型请求的一段结构化文本。
type Block struct {
	ID   string // 例如 "memory"、"coordination"
	Text string
}

// Request 是一次模型生成所需的静态 Agent 配置和动态上下文快照。
// wire 顺序是 静态角色提示 → 任务内不变的块（StableBlocks）→
// append-only 历史（Messages）→ 易变状态（StateBlocks）→ 当轮尾部（Suffix）。
// Loop 在真正请求模型前把 StateBlocks 物化成追加式 user 数据历史；后一份同 ID
// 状态取代前一份，从而既保留最新状态语义，也让后一请求逐字继承前一请求的输入前缀。
type Request struct {
	SystemPrompt string
	StableBlocks []Block
	Messages     []Message
	StateBlocks  []Block
	Suffix       string
	Tools        []agenttool.Definition
	// CacheKey 进入 Responses 的 prompt_cache_key，让同一 Agent 的请求粘在同一个缓存路由上。
	CacheKey string
}

// SetStableBlock 按 ID 覆盖或追加在一次任务内不变的前缀块。
func (r *Request) SetStableBlock(id, text string) {
	for i := range r.StableBlocks {
		if r.StableBlocks[i].ID == id {
			r.StableBlocks[i].Text = text
			return
		}
	}
	r.StableBlocks = append(r.StableBlocks, Block{ID: id, Text: text})
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
	parts := make([]string, 0, len(r.StableBlocks)+len(r.StateBlocks)+2)
	if r.SystemPrompt != "" {
		parts = append(parts, r.SystemPrompt)
	}
	for _, block := range r.StableBlocks {
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
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
	stableBlocks := make([]Block, len(request.StableBlocks))
	copy(stableBlocks, request.StableBlocks)
	return Request{
		SystemPrompt: request.SystemPrompt,
		StableBlocks: stableBlocks,
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

// SetStableSubscribedSubgraphs 设置一次任务内不变、可作为缓存前缀的记忆订阅。
func (l *Loop) SetStableSubscribedSubgraphs(ids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stableSubscribedSubgraphs = uniqueIDs(ids)
	l.memoryBlockSubs = nil
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

// unsubscribeSubgraph 从动态订阅列表里保序移除一个子图，返回是否真的移除过。
// 只过滤动态列表：package（stable）与运行时固定订阅（fixed）在结构上不可取消。
func (l *Loop) unsubscribeSubgraph(id string) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := make([]string, 0, len(l.subscribedSubgraphs))
	removed := false
	for _, existing := range l.subscribedSubgraphs {
		if existing == id {
			removed = true
			continue
		}
		kept = append(kept, existing)
	}
	if !removed {
		return false
	}
	l.subscribedSubgraphs = kept
	return true
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
		CacheKey:     l.cacheKey,
	}
}

// materializeStateBlocks 把 replaceable tail 转成有覆盖语义的追加式历史。
// 状态未变时复用已有消息，状态变化时只追加一条；不原地改写旧消息。
func (l *Loop) materializeStateBlocks(request Request) (Request, error) {
	if len(request.StateBlocks) == 0 {
		return request, nil
	}

	l.mu.Lock()
	changed := false
	for _, block := range request.StateBlocks {
		latest, found := latestContextBlock(l.messages, block.ID)
		if block.Text == "" && !found {
			continue
		}
		content := materializedStateBlock(block)
		if found && latest == content {
			continue
		}
		l.messages = append(l.messages, Message{
			Role:           RoleUser,
			Content:        content,
			ContextBlockID: block.ID,
			Timestamp:      timestampMillis(),
		})
		changed = true
	}
	request.Messages = cloneMessages(l.messages)
	request.StateBlocks = nil
	l.mu.Unlock()

	if changed {
		if err := l.persistReact(); err != nil {
			return Request{}, err
		}
	}
	return request, nil
}

func latestContextBlock(messages []Message, id string) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].ContextBlockID == id {
			return messages[i].Content, true
		}
	}
	return "", false
}

func materializedStateBlock(block Block) string {
	if block.Text == "" {
		return "（当前为空）"
	}
	return block.Text
}

// FormatSubscribedMemory 渲染一次订阅注入块。
// attribution 为真时按来源子图分组并标注多归属，让订阅者看得见边界；
// 为假时是扁平列表（当前生产默认）。两种渲染的成本差由评测直接对比。
func FormatSubscribedMemory(graph ctxgraph.Graph, subgraphIDs []string, attribution bool) string {
	return formatSubscribedNodes(graph, subgraphIDs, graph.NodesInSubgraphs(subgraphIDs), attribution)
}

func formatSubscribedNodes(
	graph ctxgraph.Graph,
	subgraphIDs []string,
	nodes []ctxgraph.Node,
	attribution bool,
) string {
	if attribution {
		return formatMemoryBySubgraph(graph, subgraphIDs, nodes)
	}
	return formatMemory(nodes)
}

// formatMemoryBySubgraph 把节点按订阅顺序归到第一张命中的子图下，并标注它的其他归属。
func formatMemoryBySubgraph(graph ctxgraph.Graph, subgraphIDs []string, nodes []ctxgraph.Node) string {
	placed := make(map[string]struct{}, len(nodes))
	var b strings.Builder
	for _, id := range subgraphIDs {
		subgraph, ok := subgraphFromGraph(graph, id)
		if !ok {
			continue
		}
		section := false
		for _, node := range nodes {
			if node.Statement == "" || !nodeInSubgraph(node, id) {
				continue
			}
			if _, done := placed[node.ID]; done && node.ID != "" {
				continue
			}
			placed[node.ID] = struct{}{}
			if b.Len() == 0 {
				b.WriteString("记忆：")
			}
			if !section {
				fmt.Fprintf(&b, "\n[%s %s]", subgraph.ID, subgraph.Name)
				section = true
			}
			writeMemoryNode(&b, node)
			if others := otherSubgraphs(node, id); len(others) > 0 {
				fmt.Fprintf(&b, "（另属 %s）", strings.Join(others, "、"))
			}
		}
	}
	return b.String()
}

func nodeInSubgraph(node ctxgraph.Node, id string) bool {
	for _, existing := range node.SubgraphIDs {
		if existing == id {
			return true
		}
	}
	return false
}

func otherSubgraphs(node ctxgraph.Node, id string) []string {
	others := make([]string, 0, len(node.SubgraphIDs))
	for _, existing := range node.SubgraphIDs {
		if existing != id {
			others = append(others, existing)
		}
	}
	return others
}

func writeMemoryNode(b *strings.Builder, node ctxgraph.Node) {
	b.WriteString("\n- ")
	if node.Kind != "" || node.Status != "" {
		fmt.Fprintf(b, "[%s/%s] ", node.Kind, node.Status)
	}
	b.WriteString(node.Statement)
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
		writeMemoryNode(&b, node)
	}
	return b.String()
}
