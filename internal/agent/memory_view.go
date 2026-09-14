package agent

import (
	"context"
	"encoding/json"
	"fmt"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// 记忆图视图的三层展开级别。级别只改变本 Agent 看到的形状，不改记忆图本身。
const (
	memoryLevelSubgraph = 1 // 只展开子图的全部字段，节点只给条数
	memoryLevelNode     = 2 // 展开节点除 statement 外的全部字段
	memoryLevelFull     = 3 // 展开节点的全部字段
)

const defaultMemoryLevel = memoryLevelSubgraph

type memoryViewNode struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind,omitempty"`
	Status         string   `json:"status,omitempty"`
	SubgraphIDs    []string `json:"subgraph_ids,omitempty"`
	SourceRefs     []string `json:"source_refs,omitempty"`
	CreatorAgentID string   `json:"creator_agent_id,omitempty"`
	SupersededBy   string   `json:"superseded_by,omitempty"`
	Statement      string   `json:"statement,omitempty"`
	Level          int      `json:"level"`
}

type memoryViewSubgraph struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind,omitempty"`
	Name      string           `json:"name,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	Admission string           `json:"admission,omitempty"`
	Scope     string           `json:"scope,omitempty"`
	Revision  int64            `json:"revision"`
	Level     int              `json:"level"`
	NodeCount int              `json:"node_count"`
	Nodes     []memoryViewNode `json:"nodes,omitempty"`
}

type memoryView struct {
	DefaultLevel    int                  `json:"default_level"`
	Levels          map[string]int       `json:"levels,omitempty"`
	Subgraphs       []memoryViewSubgraph `json:"subgraphs"`
	UnassignedCount int                  `json:"unassigned_node_count"`
	Unassigned      []memoryViewNode     `json:"unassigned_nodes,omitempty"`
}

// memoryLevels 返回当前默认级别和逐目标覆盖的副本。
func (l *Loop) memoryLevels() (int, map[string]int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	def := l.memoryViewDefault
	if def == 0 {
		def = defaultMemoryLevel
	}
	out := make(map[string]int, len(l.memoryViewLevels))
	for id, level := range l.memoryViewLevels {
		out[id] = level
	}
	return def, out
}

// setMemoryLevels 批量设置目标级别；targets 为空表示改默认级别。
// collapse 为真时同时清掉高于 level 的既有覆盖，让"全部收起"真正生效。
func (l *Loop) setMemoryLevels(targets []string, level int, collapse bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.memoryViewLevels == nil {
		l.memoryViewLevels = make(map[string]int)
	}
	if len(targets) == 0 {
		l.memoryViewDefault = level
		if !collapse {
			return
		}
		for id, existing := range l.memoryViewLevels {
			if existing > level {
				delete(l.memoryViewLevels, id)
			}
		}
		return
	}
	for _, id := range targets {
		l.memoryViewLevels[id] = level
	}
}

func (l *Loop) memorySnapshot() ctxgraph.Graph {
	l.mu.Lock()
	memory := l.memory
	l.mu.Unlock()
	if memory == nil {
		return ctxgraph.Graph{}
	}
	return memory.Snapshot()
}

func subgraphLevel(id string, def int, levels map[string]int) int {
	if level, ok := levels[id]; ok {
		return level
	}
	return def
}

// nodeLevelIn 是节点在某个子图分节下的展示级别：节点自身覆盖优先，否则跟随该子图。
// 只跟随所在分节，展开一个子图不会连带展开同一节点在别的子图里的副本。
func nodeLevelIn(node ctxgraph.Node, subgraphID string, def int, levels map[string]int) int {
	if level, ok := levels[node.ID]; ok {
		return level
	}
	return subgraphLevel(subgraphID, def, levels)
}

// effectiveNodeLevel 是节点在整张图里任意位置的最高展示级别。
// 用于无归属节点，以及判断某节点是否仍在某处展开着。
func effectiveNodeLevel(node ctxgraph.Node, def int, levels map[string]int) int {
	if level, ok := levels[node.ID]; ok {
		return level
	}
	best := 0
	for _, id := range node.SubgraphIDs {
		if level, ok := levels[id]; ok && level > best {
			best = level
		}
	}
	if best > 0 {
		return best
	}
	return def
}

func viewNode(node ctxgraph.Node, level int) memoryViewNode {
	out := memoryViewNode{
		ID:             node.ID,
		Kind:           node.Kind,
		Status:         node.Status,
		SubgraphIDs:    node.SubgraphIDs,
		SourceRefs:     node.SourceRefs,
		CreatorAgentID: node.CreatorAgentID,
		SupersededBy:   node.SupersededBy,
		Level:          level,
	}
	if level >= memoryLevelFull {
		out.Statement = node.Statement
	}
	return out
}

// buildMemoryView 按当前级别渲染三层视图：子图字段恒展开，节点按各自生效级别展开。
func buildMemoryView(graph ctxgraph.Graph, def int, levels map[string]int) memoryView {
	view := memoryView{
		DefaultLevel: def,
		Subgraphs:    make([]memoryViewSubgraph, 0, len(graph.Subgraphs)),
	}
	if len(levels) > 0 {
		view.Levels = levels
	}
	members := make(map[string][]ctxgraph.Node, len(graph.Subgraphs))
	for _, node := range graph.Nodes {
		if len(node.SubgraphIDs) == 0 {
			view.UnassignedCount++
			if level := effectiveNodeLevel(node, def, levels); level >= memoryLevelNode {
				view.Unassigned = append(view.Unassigned, viewNode(node, level))
			}
			continue
		}
		for _, id := range node.SubgraphIDs {
			members[id] = append(members[id], node)
		}
	}
	for _, subgraph := range graph.Subgraphs {
		nodes := members[subgraph.ID]
		entry := memoryViewSubgraph{
			ID:        subgraph.ID,
			Kind:      subgraph.Kind,
			Name:      subgraph.Name,
			Summary:   subgraph.Summary,
			Admission: subgraph.Admission,
			Scope:     subgraph.Scope,
			Revision:  subgraph.Revision,
			Level:     subgraphLevel(subgraph.ID, def, levels),
			NodeCount: len(nodes),
		}
		for _, node := range nodes {
			level := nodeLevelIn(node, subgraph.ID, def, levels)
			if level < memoryLevelNode {
				continue
			}
			entry.Nodes = append(entry.Nodes, viewNode(node, level))
		}
		view.Subgraphs = append(view.Subgraphs, entry)
	}
	return view
}

// collapsedNodeIDs 返回收起后不再展示 statement 的节点，用于清掉历史里的旧副本。
func collapsedNodeIDs(graph ctxgraph.Graph, def int, levels map[string]int) map[string]struct{} {
	out := make(map[string]struct{})
	for _, node := range graph.Nodes {
		if effectiveNodeLevel(node, def, levels) < memoryLevelFull {
			out[node.ID] = struct{}{}
		}
	}
	return out
}

type memoryViewTool struct {
	loop     *Loop
	collapse bool
}

var _ agenttool.Tool = memoryViewTool{}

// MemoryExpandTool 批量把子图或节点展开到指定级别。
func MemoryExpandTool(loop *Loop) agenttool.Tool {
	return memoryViewTool{loop: loop}
}

// MemoryCollapseTool 批量把子图或节点收起到指定级别，并清掉历史里的旧详情。
func MemoryCollapseTool(loop *Loop) agenttool.Tool {
	return memoryViewTool{loop: loop, collapse: true}
}

func (t memoryViewTool) Definition() agenttool.Definition {
	if t.collapse {
		return agenttool.Definition{
			Name:        memoryCollapseToolName,
			Description: "把记忆图的一批子图或节点收起到较低级别，并清掉对话历史里它们的旧详情，用来腾出上下文。级别：1 只保留子图字段（节点只剩条数），2 保留节点除 statement 外的全部字段。targets 省略或为空表示全部收起，同时把默认级别降到该级别。只改本 Agent 看到的视图，不删记忆图上的任何节点，之后可以再展开。",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"targets":{"type":"array","items":{"type":"string"},"description":"要收起的子图 ID 或节点 ID；省略或空数组表示全部"},"level":{"type":"integer","enum":[1,2],"description":"收起到的级别，默认 1"}},"additionalProperties":false}`),
		}
	}
	return agenttool.Definition{
		Name:        memoryExpandToolName,
		Description: "把记忆图的一批子图或节点展开到指定级别并返回展开后的视图。级别：1 只展开子图自身的全部字段（id、kind、name、summary、admission、scope、revision）和成员节点条数；2 额外展开每个节点除 statement 外的全部字段（id、kind、status、subgraph_ids、source_refs、creator_agent_id、superseded_by）；3 再加上 statement 全文。先用 1 看清有哪些子图，再对可能相关的子图用 2 看节点头，最后只对真正要审的节点用 3 取全文——不要一上来就对全图用 3。targets 省略或为空表示全部，同时把默认级别设为该级别。只改本 Agent 看到的视图，不改记忆图。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"targets":{"type":"array","items":{"type":"string"},"description":"要展开的子图 ID 或节点 ID；省略或空数组表示全部"},"level":{"type":"integer","enum":[1,2,3],"description":"展开到的级别，默认 3"}},"additionalProperties":false}`),
	}
}

func (t memoryViewTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	name := t.name()
	if t.loop == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil loop", name)
	}

	var args struct {
		Targets []string `json:"targets"`
		Level   *int     `json:"level"`
	}
	raw := call.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return agenttool.Output{}, fmt.Errorf("decode arguments: %w", err)
	}

	level, err := t.resolveLevel(args.Level)
	if err != nil {
		return agenttool.Output{}, err
	}
	targets := uniqueIDs(args.Targets)

	graph := t.loop.memorySnapshot()
	if err := validateTargets(name, graph, targets); err != nil {
		return agenttool.Output{}, err
	}

	t.loop.setMemoryLevels(targets, level, t.collapse)
	def, levels := t.loop.memoryLevels()
	if t.collapse {
		t.loop.dropNodesFromMessages(collapsedNodeIDs(graph, def, levels))
	}

	payload, err := json.Marshal(buildMemoryView(graph, def, levels))
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode memory view: %w", err)
	}
	return agenttool.Output{Content: string(payload)}, nil
}

func (t memoryViewTool) name() string {
	if t.collapse {
		return memoryCollapseToolName
	}
	return memoryExpandToolName
}

func (t memoryViewTool) resolveLevel(requested *int) (int, error) {
	high := memoryLevelFull
	fallback := memoryLevelFull
	if t.collapse {
		high = memoryLevelNode
		fallback = memoryLevelSubgraph
	}
	if requested == nil {
		return fallback, nil
	}
	if *requested < memoryLevelSubgraph || *requested > high {
		return 0, fmt.Errorf("%s: level %d out of range [%d,%d]", t.name(), *requested, memoryLevelSubgraph, high)
	}
	return *requested, nil
}

// validateTargets 拒绝图上不存在的 ID，避免模型用臆造 ID 静默改不到任何东西。
func validateTargets(name string, graph ctxgraph.Graph, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(graph.Nodes)+len(graph.Subgraphs))
	for _, subgraph := range graph.Subgraphs {
		known[subgraph.ID] = struct{}{}
	}
	for _, node := range graph.Nodes {
		known[node.ID] = struct{}{}
	}
	for _, id := range targets {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("%s: unknown subgraph or node %q", name, id)
		}
	}
	return nil
}
