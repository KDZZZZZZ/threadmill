package agent

import (
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

// sessionResetHandoff 是会话重置后放在新会话最前面的交接抬头。
// organizer 的持久状态全部住在记忆图里（子图说明、节点状态、归属），消息历史只是可丢弃的缓存：
// 窗口压力下与其把对话压进图（compact 只增不改，会污染正在整理的图），不如丢掉历史、
// 从图重新实例化工作状态。交接只带机械可得的信息（本次会话经手的子图 ID 与它们在图上的说明），
// 不额外请求模型写小结——那会多一次调用并作废整段前缀缓存。
const sessionResetHandoff = "会话已重置：为控制上下文成本，本次会话更早的对话历史（工具结果与推理过程）已经丢弃。" +
	"记忆图是唯一的持久状态，下面的清单直接来自图；没有写进图的判断已经丢失。" +
	"继续按当前查询工作：需要早前的结论时用 memory_expand / memory_nodes_in 从图上重新取，" +
	"并把本次的判断和排除理由经 describe_subgraph 落回图上，下一次重置才不会再丢一遍。"

// noteSessionSubgraph 记下本次会话经手过的目标子图，供重置后的交接清单使用。
func (l *Loop) noteSessionSubgraph(id string) {
	if l == nil || id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.sessionSubgraphs {
		if existing == id {
			return
		}
	}
	l.sessionSubgraphs = append(l.sessionSubgraphs, id)
}

// shouldResetBeforeRequest 判断本次请求是否该先丢掉当前回合之前的历史。
// 只对开启了会话重置的 Agent（整理 Agent）生效，且必须真有可丢的历史前缀。
func (l *Loop) shouldResetBeforeRequest(request Request) bool {
	if l == nil || !l.sessionReset || l.contextWindow <= 0 ||
		estimateRequestTokens(request) < softContextThreshold(l.contextWindow) {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	start := currentTurnStart(l.messages)
	// 当前回合自己就撑破了阈值：前面只剩上一次的交接消息，再重置一次什么也丢不掉，
	// 只会每次生成都重写一遍抬头。这种压力交给 drop/collapse 处理。
	if start == 1 && l.sessionHandoffHead {
		return false
	}
	return start > 0
}

// resetSession 把消息历史截到当前回合的起点，并在最前面放一条交接消息。
// 只截当前 user 消息之前的部分：当前回合内的 tool_call/tool_result 配对保持完整，
// 重置因此既可以发生在回合之间，也可以发生在一次整理的中途。
func (l *Loop) resetSession() error {
	graph := l.memorySnapshot()
	l.mu.Lock()
	start := currentTurnStart(l.messages)
	if start <= 0 {
		l.mu.Unlock()
		return nil
	}
	handoff := Message{
		Role:      RoleUser,
		Content:   sessionHandoffText(l.sessionResetPrompt, l.sessionSubgraphs, graph),
		Timestamp: timestampMillis(),
	}
	l.messages = append([]Message{handoff}, l.messages[start:]...)
	l.sessionHandoffHead = true
	l.mu.Unlock()
	return l.persistReact()
}

// currentTurnStart 返回当前回合第一条消息的下标：最后一条真正的用户消息。
// 状态块物化出来的 user 消息带 ContextBlockID，工具结果是 RoleTool，都不算回合起点。
func currentTurnStart(messages []Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser &&
			messages[i].ContextBlockID == "" &&
			messages[i].ToolResult == nil {
			return i
		}
	}
	return 0
}

func sessionHandoffText(prompt string, ids []string, graph ctxgraph.Graph) string {
	if prompt == "" {
		prompt = sessionResetHandoff
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n本次会话经手过的子图：\n")
	if len(ids) == 0 {
		b.WriteString("（无）\n")
		return b.String()
	}
	for _, id := range ids {
		subgraph, ok := subgraphFromGraph(graph, id)
		if !ok {
			fmt.Fprintf(&b, "- %s（已不在图上）\n", id)
			continue
		}
		fmt.Fprintf(&b, "- %s name=%s\n", subgraph.ID, subgraph.Name)
		writeSubgraphDescription(&b, subgraph, "  ")
	}
	return b.String()
}
