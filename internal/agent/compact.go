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
// subgraphIDs 是本 agent 当前订阅，只影响来源边和整理提示里的「已有记忆」；
// 节点归属可以是图中任意已有子图，也可以为空。agentID 写入 CreatorAgentID，并用于创建者链。
func CompactHistory(
	ctx context.Context,
	provider Provider,
	graph ctxgraph.Graph,
	messages []Message,
	subgraphIDs []string,
	keepRecentTokens int,
	agentID string,
) (ctxgraph.Graph, []Message, error) {
	subgraphIDs = uniqueIDs(subgraphIDs)

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
	if last, ok := graph.LastNodeOfCreator(agentID); ok {
		previousID = last.ID
	}
	nodes, edges := nodesFromDrafts(
		drafts,
		subgraphIDs,
		allSubgraphIDs(graph),
		previousID,
		agentID,
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
			SystemPrompt: compactSystemPrompt(ctx),
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
			Message{Role: RoleUser, Content: compactJSONReminder(ctx) + "\n解析错误：" + err.Error()},
		)
	}
	return nil, fmt.Errorf("organizing memory: %w", lastParse)
}

func compactSystemPrompt(ctx context.Context) string {
	transcript, ok := TranscriptFromContext(ctx)
	if !ok {
		return ""
	}
	return transcript.CompactPrompt
}

func compactJSONReminder(ctx context.Context) string {
	transcript, ok := TranscriptFromContext(ctx)
	if !ok {
		return ""
	}
	return transcript.CompactJSONReminder
}

func buildOrganizeUserPrompt(graph ctxgraph.Graph, subgraphIDs []string, messages []Message) string {
	var b strings.Builder
	b.WriteString("当前订阅：\n")
	writeSubgraphCatalog(&b, graph, subgraphIDs)
	b.WriteString("\n可选归属子图：\n")
	writeSubgraphCatalog(&b, graph, allSubgraphIDs(graph))
	b.WriteString("\n已有记忆：\n")
	existing := graph.NodesInSubgraphs(subgraphIDs)
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

func allSubgraphIDs(graph ctxgraph.Graph) []string {
	ids := make([]string, 0, len(graph.Subgraphs))
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
	agentID string,
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
		node := ctxgraph.Node{
			ID:             id,
			Kind:           normalizeKind(draft.Kind),
			Statement:      statement,
			Status:         normalizeStatus(draft.Status),
			SubgraphIDs:    members,
			CreatorAgentID: agentID,
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
