package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

const defaultKeepRecentTokens = 20000
const maxOrganizeFormatAttempts = 3

const organizeJSONReminder = `上次输出不是完整 JSON。只输出一个 JSON 对象，不要 markdown，不要其它文字。格式：{"nodes":[{"kind":"fact","statement":"...","status":"accepted","subgraph_ids":["sg-a"]}]}`

// OrganizePrompt 是把对话整理成记忆节点的系统提示词（初稿）。
const OrganizePrompt = `你是记忆整理器。把一段对话整理成记忆图节点。不要继续对话，不要回答用户。

规则：
- 只提取之后还用得上的知识：目标、约束、已确认事实、未决假设、关键决策、文件/符号/错误信息。
- 不要一对一地为每条消息建节点。能合并的合并；寒暄、重复、中间推理、无结果的工具过程丢掉。
- 一条陈述只写一件事，短句，保留确切路径、名称和错误原文。
- kind 只能是 directive（约束/偏好）、fact（已成立）、hypothesis（待验证）。
- status 只能是 accepted、disputed、superseded、outdated。
- subgraph_ids 只能从用户消息里给出的子图 ID 中选：按内容判断这条知识正式属于哪些子图。不知道就用当前订阅。
- 不要填写来源子图；来源由当前订阅列表决定。
- 不要重复「已有记忆」里已经有的陈述。
- 只输出 JSON，不要 markdown，不要其它文字。

格式：
{"nodes":[{"kind":"fact","statement":"...","status":"accepted","subgraph_ids":["sg-a"]}]}`

type organizeOutput struct {
	Nodes []organizeNode `json:"nodes"`
}

type organizeNode struct {
	Kind        string   `json:"kind"`
	Statement   string   `json:"statement"`
	Status      string   `json:"status"`
	SubgraphIDs []string `json:"subgraph_ids"`
}

// CompactHistory 将切点之前的消息整理进记忆图，并返回保留的尾部消息。
func CompactHistory(
	ctx context.Context,
	provider Provider,
	graph ctxgraph.Graph,
	messages []Message,
	subgraphIDs []string,
	keepRecentTokens int,
) (ctxgraph.Graph, []Message, error) {
	subgraphIDs = uniqueIDs(subgraphIDs)
	if len(subgraphIDs) == 0 {
		return graph.Clone(), cloneMessages(messages), nil
	}

	cut := keepRecentIndex(messages, keepRecentTokens)
	if cut == 0 {
		return graph.Clone(), cloneMessages(messages), nil
	}

	drafts, err := organizeWithModel(
		ctx,
		provider,
		graph,
		subgraphIDs,
		messages[:cut],
	)
	if err != nil {
		return ctxgraph.Graph{}, nil, err
	}

	previousID := ""
	if existing := graph.NodesInSubgraphs(subgraphIDs); len(existing) > 0 {
		previousID = existing[len(existing)-1].ID
	}
	nodes, edges := nodesFromDrafts(
		drafts,
		subgraphIDs,
		catalogIDs(graph, subgraphIDs),
		previousID,
		graph.Nodes,
	)
	tail := cloneMessages(messages[cut:])
	if len(nodes) == 0 {
		return graph.Clone(), tail, nil
	}
	return graph.WithMemory(nodes, edges), tail, nil
}

func organizeWithModel(
	ctx context.Context,
	provider Provider,
	graph ctxgraph.Graph,
	subgraphIDs []string,
	history []Message,
) ([]organizeNode, error) {
	if provider == nil {
		return nil, fmt.Errorf("organizing memory: %w", ErrNilProvider)
	}
	requestMessages := []Message{{
		Role:    RoleUser,
		Content: buildOrganizeUserPrompt(graph, subgraphIDs, history),
	}}
	var lastParse error
	for range maxOrganizeFormatAttempts {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("organizing memory: %w", err)
		}
		response, err := provider.Generate(ctx, Request{
			SystemPrompt: OrganizePrompt,
			Messages:     requestMessages,
		})
		if err != nil {
			return nil, fmt.Errorf("organizing memory: %w", err)
		}
		drafts, err := parseOrganizeOutput(response.Content)
		if err == nil {
			return drafts, nil
		}
		lastParse = err
		requestMessages = append(requestMessages,
			Message{Role: RoleAssistant, Content: response.Content},
			Message{Role: RoleUser, Content: organizeJSONReminder + "\n解析错误：" + err.Error()},
		)
	}
	return nil, fmt.Errorf("organizing memory: %w", lastParse)
}

func buildOrganizeUserPrompt(graph ctxgraph.Graph, subgraphIDs []string, messages []Message) string {
	catalog := catalogIDs(graph, subgraphIDs)
	var b strings.Builder
	b.WriteString("当前订阅：\n")
	writeSubgraphCatalog(&b, graph, subgraphIDs)
	b.WriteString("\n可选归属子图：\n")
	writeSubgraphCatalog(&b, graph, catalog)
	b.WriteString("\n已有记忆：\n")
	existing := graph.NodesInSubgraphs(catalog)
	if len(existing) == 0 {
		b.WriteString("（无）\n")
	}
	for _, node := range existing {
		fmt.Fprintf(&b, "- [%s/%s] %s\n", node.Kind, node.Status, node.Statement)
	}
	b.WriteString("\n待整理对话：\n")
	b.WriteString(serializeConversation(messages))
	return b.String()
}

func writeSubgraphCatalog(b *strings.Builder, graph ctxgraph.Graph, ids []string) {
	if len(ids) == 0 {
		b.WriteString("（无）\n")
		return
	}
	byID := make(map[string]ctxgraph.Subgraph, len(graph.Subgraphs))
	for _, subgraph := range graph.Subgraphs {
		byID[subgraph.ID] = subgraph
	}
	for _, id := range ids {
		if subgraph, ok := byID[id]; ok {
			fmt.Fprintf(b, "- %s kind=%s name=%s %s\n", id, subgraph.Kind, subgraph.Name, subgraph.Summary)
			continue
		}
		fmt.Fprintf(b, "- %s\n", id)
	}
}

func catalogIDs(graph ctxgraph.Graph, subscribed []string) []string {
	ids := append([]string(nil), subscribed...)
	for _, subgraph := range graph.Subgraphs {
		ids = append(ids, subgraph.ID)
	}
	return uniqueIDs(ids)
}

func parseOrganizeOutput(content string) ([]organizeNode, error) {
	raw := extractJSONObject(content)
	if raw == nil {
		return nil, fmt.Errorf("missing json object")
	}
	var output organizeOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		return nil, err
	}
	return output.Nodes, nil
}

func extractJSONObject(content string) []byte {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return nil
	}
	return []byte(content[start : end+1])
}

func nodesFromDrafts(
	drafts []organizeNode,
	subscribed []string,
	allowedMemberIDs []string,
	previousNodeID string,
	existing []ctxgraph.Node,
) ([]ctxgraph.Node, []ctxgraph.Edge) {
	allowedMembers := make(map[string]struct{}, len(allowedMemberIDs))
	for _, id := range allowedMemberIDs {
		allowedMembers[id] = struct{}{}
	}

	used := make(map[string]struct{}, len(existing)+len(drafts))
	for _, node := range existing {
		if node.ID != "" {
			used[node.ID] = struct{}{}
		}
	}

	nodes := make([]ctxgraph.Node, 0)
	edges := make([]ctxgraph.Edge, 0)
	prev := previousNodeID
	next := 1
	for _, draft := range drafts {
		statement := strings.TrimSpace(draft.Statement)
		if statement == "" {
			continue
		}
		id := nextMemoryNodeID(used, &next)
		members := make([]string, 0, len(draft.SubgraphIDs))
		for _, memberID := range uniqueIDs(draft.SubgraphIDs) {
			if _, ok := allowedMembers[memberID]; !ok {
				continue
			}
			members = append(members, memberID)
		}
		if len(members) == 0 {
			members = append([]string(nil), subscribed...)
		}
		node := ctxgraph.Node{
			ID:          id,
			Kind:        normalizeKind(draft.Kind),
			Statement:   statement,
			Status:      normalizeStatus(draft.Status),
			SubgraphIDs: members,
		}
		nodes = append(nodes, node)
		if prev != "" {
			edges = append(edges, ctxgraph.Edge{
				FromRef:  ctxgraph.NodeRef(prev),
				ToNodeID: id,
				Kind:     ctxgraph.EdgeKindLogicalAdjacent,
			})
		}
		for _, sourceID := range subscribed {
			if sourceID == "" {
				continue
			}
			edges = append(edges, ctxgraph.Edge{
				FromRef:  ctxgraph.SubgraphRef(sourceID),
				ToNodeID: id,
				Kind:     ctxgraph.EdgeKindDerivesFromSubgraph,
			})
		}
		prev = id
	}
	return nodes, edges
}

func normalizeKind(kind string) string {
	switch kind {
	case ctxgraph.NodeKindDirective, ctxgraph.NodeKindFact, ctxgraph.NodeKindHypothesis:
		return kind
	default:
		return ctxgraph.NodeKindFact
	}
}

func normalizeStatus(status string) string {
	switch status {
	case ctxgraph.NodeStatusAccepted, ctxgraph.NodeStatusDisputed, ctxgraph.NodeStatusSuperseded, ctxgraph.NodeStatusOutdated:
		return status
	default:
		return ctxgraph.NodeStatusAccepted
	}
}

func serializeConversation(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case RoleUser:
			if text := strings.TrimSpace(message.Content); text != "" {
				parts = append(parts, "[User]: "+text)
			}
		case RoleAssistant:
			if text := strings.TrimSpace(message.Thinking); text != "" {
				parts = append(parts, "[Assistant thinking]: "+text)
			}
			if text := strings.TrimSpace(message.Content); text != "" {
				parts = append(parts, "[Assistant]: "+text)
			}
			if len(message.ToolCalls) > 0 {
				calls := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					args := strings.TrimSpace(string(call.Arguments))
					if args == "" {
						args = "{}"
					}
					calls = append(calls, fmt.Sprintf("%s(%s)", call.Name, compactJSON(args)))
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case RoleTool:
			if text := strings.TrimSpace(message.Content); text != "" {
				parts = append(parts, "[Tool result]: "+text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func keepRecentBudget(contextWindow int) int {
	keep := defaultKeepRecentTokens
	if contextWindow > 0 && keep > contextWindow/2 {
		keep = contextWindow / 2
	}
	if keep < 1 {
		keep = 1
	}
	return keep
}

func keepRecentIndex(messages []Message, keepRecentTokens int) int {
	if keepRecentTokens <= 0 {
		return len(messages)
	}
	accumulated := 0
	for i := len(messages) - 1; i >= 0; i-- {
		accumulated += estimateTokens(messages[i])
		if accumulated < keepRecentTokens {
			continue
		}
		return lastCutPointAtOrBefore(messages, i)
	}
	return 0
}

func lastCutPointAtOrBefore(messages []Message, index int) int {
	for i := index; i >= 0; i-- {
		if isCutPoint(messages[i]) {
			return i
		}
	}
	return 0
}

func isCutPoint(message Message) bool {
	return message.Role == RoleUser || message.Role == RoleAssistant
}

func estimateTokens(message Message) int {
	chars := len(message.Content) + len(message.Thinking)
	for _, call := range message.ToolCalls {
		chars += len(call.Name) + len(call.Arguments)
	}
	if message.ToolResult != nil {
		chars += len(message.ToolResult.Content) + len(message.ToolResult.Details)
	}
	if chars == 0 {
		return 0
	}
	return (chars + 3) / 4
}

func compactJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func nextMemoryNodeID(used map[string]struct{}, next *int) string {
	for {
		id := fmt.Sprintf("mem-%d", *next)
		*next++
		if _, exists := used[id]; exists {
			continue
		}
		used[id] = struct{}{}
		return id
	}
}

func uniqueIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
