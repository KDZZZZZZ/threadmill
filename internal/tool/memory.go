package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const (
	memoryNeighborsName     = "memory_neighbors"
	memorySubgraphsOfName   = "memory_subgraphs_of"
	memorySourcesOfName     = "memory_sources_of"
	memoryNodesInName       = "memory_nodes_in"
	memoryAddToSubgraphName = "memory_add_to_subgraph"
)

type memoryNode struct {
	ID        string `json:"id"`
	Kind      string `json:"kind,omitempty"`
	Statement string `json:"statement,omitempty"`
	Status    string `json:"status,omitempty"`
}

type memoryTool struct {
	name     string
	snapshot func() ctxgraph.Copy
	commit   func(ctxgraph.Copy) error
}

var (
	_ Tool      = memoryTool{}
	_ EnvBinder = memoryTool{}
)

// MemoryTools 返回操作本 Agent 已持有图副本的工具。
// snapshot 必须返回那份副本；写入工具通过 commit 写回，不得再 Clone 全局图。
func MemoryTools(snapshot func() ctxgraph.Copy, commit func(ctxgraph.Copy) error) []Tool {
	return []Tool{
		memoryTool{name: memoryNeighborsName, snapshot: snapshot, commit: commit},
		memoryTool{name: memorySubgraphsOfName, snapshot: snapshot, commit: commit},
		memoryTool{name: memorySourcesOfName, snapshot: snapshot, commit: commit},
		memoryTool{name: memoryNodesInName, snapshot: snapshot, commit: commit},
		memoryTool{name: memoryAddToSubgraphName, snapshot: snapshot, commit: commit},
	}
}

func (t memoryTool) Definition() Definition {
	switch t.name {
	case memoryNeighborsName:
		return Definition{
			Name:        memoryNeighborsName,
			Description: "查看某记忆节点的上游（前）和下游（后）节点。before/after：负数从该方向列表最前取 |n| 个，正数从最后取 n 个；省略则返回该方向全部节点。只读本 Agent 的图副本。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"node_id":{"type":"string","description":"记忆节点 ID"},"before":{"type":"integer","description":"上游窗口：负数从最前取，正数从最后取"},"after":{"type":"integer","description":"下游窗口：负数从最前取，正数从最后取"}},"required":["node_id"],"additionalProperties":false}`),
		}
	case memorySubgraphsOfName:
		return Definition{
			Name:        memorySubgraphsOfName,
			Description: "查看某记忆节点正式所属的子图 ID。只读本 Agent 的图副本。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"node_id":{"type":"string","description":"记忆节点 ID"}},"required":["node_id"],"additionalProperties":false}`),
		}
	case memorySourcesOfName:
		return Definition{
			Name:        memorySourcesOfName,
			Description: "查看某记忆节点由哪些子图产生（来源子图）。只读本 Agent 的图副本。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"node_id":{"type":"string","description":"记忆节点 ID"}},"required":["node_id"],"additionalProperties":false}`),
		}
	case memoryNodesInName:
		return Definition{
			Name:        memoryNodesInName,
			Description: "按子图查看记忆节点，返回至少属于其中一个子图的节点并集。只读本 Agent 的图副本。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"subgraph_ids":{"type":"array","items":{"type":"string"},"description":"子图 ID 列表"}},"required":["subgraph_ids"],"additionalProperties":false}`),
		}
	default:
		return Definition{
			Name:        memoryAddToSubgraphName,
			Description: "把指定记忆节点加入某个子图。只改本 Agent 的图副本。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"subgraph_id":{"type":"string","description":"目标子图 ID"},"node_ids":{"type":"array","items":{"type":"string"},"description":"要加入该子图的节点 ID"}},"required":["subgraph_id","node_ids"],"additionalProperties":false}`),
		}
	}
}

func (t memoryTool) BindEnv(e env.Env) Tool {
	t.snapshot = func() ctxgraph.Copy {
		if e.Memory == nil {
			return ctxgraph.Copy{}
		}
		return ctxgraph.Copy{Graph: e.Memory.Snapshot()}
	}
	t.commit = func(copy ctxgraph.Copy) error {
		if e.Memory == nil {
			return nil
		}
		return e.Memory.Commit(copy.Graph)
	}
	return t
}

func (t memoryTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if t.snapshot == nil {
		return Output{}, fmt.Errorf("%s: not bound to env", t.name)
	}

	switch t.name {
	case memoryNeighborsName:
		return t.neighbors(t.copyGraph(), call.Arguments)
	case memorySubgraphsOfName:
		return t.subgraphsOf(t.copyGraph(), call.Arguments)
	case memorySourcesOfName:
		return t.sourcesOf(t.copyGraph(), call.Arguments)
	case memoryNodesInName:
		return t.nodesIn(t.copyGraph(), call.Arguments)
	default:
		return t.addToSubgraph(call.Arguments)
	}
}

func (t memoryTool) copyGraph() ctxgraph.Graph {
	if t.snapshot == nil {
		return ctxgraph.Graph{}
	}
	return t.snapshot().Graph
}

func (t memoryTool) neighbors(graph ctxgraph.Graph, raw json.RawMessage) (Output, error) {
	var args struct {
		NodeID string `json:"node_id"`
		Before *int   `json:"before"`
		After  *int   `json:"after"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(args.NodeID) == "" {
		return Output{}, fmt.Errorf("%s: missing node_id", t.name)
	}

	return marshalMemory(struct {
		Before []memoryNode `json:"before"`
		After  []memoryNode `json:"after"`
	}{
		Before: compactMemoryNodes(takeSigned(reachableInGraphOrder(graph, args.NodeID, true), args.Before)),
		After:  compactMemoryNodes(takeSigned(reachableInGraphOrder(graph, args.NodeID, false), args.After)),
	})
}

func (t memoryTool) subgraphsOf(graph ctxgraph.Graph, raw json.RawMessage) (Output, error) {
	nodeID, err := decodeNodeID(t.name, raw)
	if err != nil {
		return Output{}, err
	}
	ids := graph.SubgraphsOf(nodeID)
	if ids == nil {
		ids = []string{}
	}
	return marshalMemory(struct {
		SubgraphIDs []string `json:"subgraph_ids"`
	}{SubgraphIDs: ids})
}

func (t memoryTool) sourcesOf(graph ctxgraph.Graph, raw json.RawMessage) (Output, error) {
	nodeID, err := decodeNodeID(t.name, raw)
	if err != nil {
		return Output{}, err
	}
	ids := graph.SourceSubgraphsOf(nodeID)
	if ids == nil {
		ids = []string{}
	}
	return marshalMemory(struct {
		SourceSubgraphIDs []string `json:"source_subgraph_ids"`
	}{SourceSubgraphIDs: ids})
}

func (t memoryTool) nodesIn(graph ctxgraph.Graph, raw json.RawMessage) (Output, error) {
	var args struct {
		SubgraphIDs []string `json:"subgraph_ids"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	return marshalMemory(struct {
		Nodes []memoryNode `json:"nodes"`
	}{Nodes: compactMemoryNodes(graph.NodesInSubgraphs(args.SubgraphIDs))})
}

func (t memoryTool) addToSubgraph(raw json.RawMessage) (Output, error) {
	var args struct {
		SubgraphID string   `json:"subgraph_id"`
		NodeIDs    []string `json:"node_ids"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return Output{}, err
	}
	if strings.TrimSpace(args.SubgraphID) == "" {
		return Output{}, fmt.Errorf("%s: missing subgraph_id", t.name)
	}
	if len(args.NodeIDs) == 0 {
		return Output{}, fmt.Errorf("%s: missing node_ids", t.name)
	}
	if t.commit == nil {
		return Output{}, fmt.Errorf("%s: missing copy commit", t.name)
	}

	copy := ctxgraph.Copy{}
	if t.snapshot != nil {
		copy = t.snapshot()
	}
	for _, subgraph := range copy.Graph.Subgraphs {
		if subgraph.ID == args.SubgraphID && subgraph.Kind == ctxgraph.SubgraphKindSystem {
			return Output{}, fmt.Errorf("%s: system subgraph is runtime-managed", t.name)
		}
	}

	known := make(map[string]struct{}, len(copy.Graph.Nodes))
	for _, node := range copy.Graph.Nodes {
		if node.ID != "" {
			known[node.ID] = struct{}{}
		}
	}

	added := make([]string, 0)
	missing := make([]string, 0)
	seen := make(map[string]struct{}, len(args.NodeIDs))
	for _, id := range args.NodeIDs {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if _, ok := known[id]; !ok {
			missing = append(missing, id)
			continue
		}
		added = append(added, id)
	}

	copy.Graph = copy.Graph.WithNodesInSubgraph(args.SubgraphID, added)
	if err := t.commit(copy); err != nil {
		return Output{}, err
	}
	return marshalMemory(struct {
		SubgraphID string   `json:"subgraph_id"`
		Added      []string `json:"added"`
		Missing    []string `json:"missing"`
	}{
		SubgraphID: args.SubgraphID,
		Added:      added,
		Missing:    missing,
	})
}

func decodeNodeID(name string, raw json.RawMessage) (string, error) {
	var args struct {
		NodeID string `json:"node_id"`
	}
	if err := decodeMemoryArgs(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.NodeID) == "" {
		return "", fmt.Errorf("%s: missing node_id", name)
	}
	return args.NodeID, nil
}

func decodeMemoryArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

func marshalMemory(v any) (Output, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return Output{}, fmt.Errorf("encode memory tool output: %w", err)
	}
	return Output{Content: string(payload)}, nil
}

func compactMemoryNodes(nodes []ctxgraph.Node) []memoryNode {
	out := make([]memoryNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, memoryNode{
			ID:        node.ID,
			Kind:      node.Kind,
			Statement: node.Statement,
			Status:    node.Status,
		})
	}
	return out
}

func reachableInGraphOrder(graph ctxgraph.Graph, start string, upstream bool) []ctxgraph.Node {
	seen := map[string]struct{}{start: {}}
	found := make(map[string]struct{})
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		var next []ctxgraph.Node
		if upstream {
			next = graph.UpstreamNodes(id)
		} else {
			next = graph.DownstreamNodes(id)
		}
		for _, node := range next {
			if node.ID == "" {
				continue
			}
			if _, ok := seen[node.ID]; ok {
				continue
			}
			seen[node.ID] = struct{}{}
			found[node.ID] = struct{}{}
			queue = append(queue, node.ID)
		}
	}

	nodes := make([]ctxgraph.Node, 0, len(found))
	for _, node := range graph.Nodes {
		if _, ok := found[node.ID]; !ok {
			continue
		}
		delete(found, node.ID)
		nodes = append(nodes, node)
	}
	return nodes
}

func takeSigned[T any](items []T, n *int) []T {
	if n == nil {
		if items == nil {
			return []T{}
		}
		return items
	}
	count := *n
	if count == 0 || len(items) == 0 {
		return []T{}
	}
	if count < 0 {
		count = -count
		if count > len(items) {
			count = len(items)
		}
		return append([]T(nil), items[:count]...)
	}
	if count > len(items) {
		count = len(items)
	}
	return append([]T(nil), items[len(items)-count:]...)
}
