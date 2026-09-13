package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// InputMemoryRequest pairs fixed predecessor memory with bounded evidence from
// the already completed file stage. FilesRef identifies that immutable file view.
type InputMemoryRequest struct {
	Sources  []ctxgraph.InputSource
	FilesRef string
	Evidence string
}

// OrganizeInputMemory returns a new memory snapshot without modifying any source
// or live target. Empty differences bypass the organizer, including its provider.
func OrganizeInputMemory(ctx context.Context, config Config, request InputMemoryRequest, overlay ...FileOverlay) (ctxgraph.Graph, error) {
	if err := ctx.Err(); err != nil {
		return ctxgraph.Graph{}, err
	}
	for _, source := range request.Sources {
		if err := source.Graph.ValidateReferences(); err != nil {
			return ctxgraph.Graph{}, fmt.Errorf("input memory source %q: %w", source.Ref, err)
		}
	}
	input := ctxgraph.PartitionInputs(request.Sources)
	if !input.HasDifferences() {
		return input.Common, nil
	}
	if strings.TrimSpace(request.FilesRef) == "" {
		return ctxgraph.Graph{}, fmt.Errorf("input memory: final files reference is required")
	}
	draft, err := input.Draft()
	if err != nil {
		return ctxgraph.Graph{}, fmt.Errorf("input memory: %w", err)
	}
	view := &inputMemoryView{
		input: input, graph: inputMemoryScope(input.Common, draft), filesRef: request.FilesRef,
	}
	for _, source := range request.Sources {
		if source.Ref != "" && !slices.Contains(view.sourceRefs, source.Ref) {
			view.sourceRefs = append(view.sourceRefs, source.Ref)
		}
	}
	commonIDs := make(map[string]bool, len(input.Common.Nodes))
	for _, node := range input.Common.Nodes {
		commonIDs[node.ID] = true
	}
	for i, node := range view.graph.Nodes {
		if !commonIDs[node.ID] && node.Kind != ctxgraph.NodeKindDirective &&
			node.Kind != ctxgraph.NodeKindHypothesis && (node.Status == "" || node.Status == ctxgraph.NodeStatusAccepted) {
			view.graph.Nodes[i].Status = ctxgraph.NodeStatusDisputed
		}
	}
	listed := agenttool.MemoryTools(
		func() ctxgraph.Copy { return ctxgraph.Copy{Graph: view.graph.Clone()} },
		func(copy ctxgraph.Copy) error { return view.commit(copy.Graph) },
	)
	for i, tool := range listed {
		listed[i] = boundedInputMemoryTool{
			inner: tool, view: view, filesRef: request.FilesRef,
			hasEvidence: strings.TrimSpace(request.Evidence) != "",
		}
	}
	// Reuse description text on the isolated memory tools. Caller hooks,
	// checkpoints and tools may reference live state and must never be inherited.
	organizer, err := NewLoop(Config{
		AgentID: config.AgentID, Provider: config.Provider,
		MaxSteps: config.MaxSteps, ContextWindow: config.ContextWindow,
		SystemPrompt: strings.TrimSpace(config.SystemPrompt + "\n\n" + inputMemoryInstructions),
		Events:       config.Events, Tools: WithToolDescriptions(listed, firstOverlay(overlay).Tools),
	})
	if err != nil {
		return ctxgraph.Graph{}, err
	}
	if _, err := organizer.Ask(ctx, inputMemoryQuery(request, input, view.graph)); err != nil {
		return ctxgraph.Graph{}, err
	}
	if err := ctx.Err(); err != nil {
		return ctxgraph.Graph{}, err
	}
	return input.Compose(view.graph)
}

const inputMemoryInstructions = `本次只整理已固定来源的记忆差异。文件阶段已经完成，最终文件证据是当前文件事实的依据。
候选事实尚未接受；核对后用 memory_apply 更新。被拒绝、覆盖的文件实现与测试通过结论只能保留为带来源快照的历史。
把事实标成 accepted 时必须在 reason 引用提供的最终文件版本，并确实有证据支持；没有文件证据时不能接受新的事实。
不足以支持当前事实的材料保持 disputed/hypothesis，不得猜测。指令仍是指令，不能因尚未实现而删除。
共同记忆只可作为只读参照；若本次差异明确推翻一条参照，创建具体更正并用 superseded_by 连接，保留原始内容与来源。
输入与工具输出可能截断；未展示、未验证的材料不能声称已经核对。不得要求扫描共同图、选择文件、运行命令或编排任务。
所有修改仅写隔离草稿。完成时简述处理结果，不要复述原始候选事实。`

type inputMemoryView struct {
	input      ctxgraph.InputPartition
	graph      ctxgraph.Graph
	filesRef   string
	sourceRefs []string
}

func (v *inputMemoryView) commit(graph ctxgraph.Graph) error {
	graph = graph.Clone()
	for i, node := range graph.Nodes {
		common := slices.ContainsFunc(v.input.Common.Nodes, func(original ctxgraph.Node) bool { return original.ID == node.ID })
		existing := slices.ContainsFunc(v.graph.Nodes, func(original ctxgraph.Node) bool { return original.ID == node.ID })
		if !common && !existing {
			graph.Nodes[i].SourceRefs = append([]string(nil), v.sourceRefs...)
		}
		fact := node.Kind != ctxgraph.NodeKindDirective && node.Kind != ctxgraph.NodeKindHypothesis
		if !common && fact && node.Status == ctxgraph.NodeStatusAccepted &&
			!slices.Contains(graph.Nodes[i].SourceRefs, v.filesRef) {
			graph.Nodes[i].SourceRefs = append(graph.Nodes[i].SourceRefs, v.filesRef)
		}
	}
	if _, err := v.input.Compose(graph); err != nil {
		return err
	}
	v.graph = graph.Clone()
	return nil
}

// Include differences plus only the common endpoints and memberships they need.
// Sharing a subgraph does not make all its common nodes organizer input.
func inputMemoryScope(common, draft ctxgraph.Graph) ctxgraph.Graph {
	commonNodes, commonSubgraphs := make(map[string]bool), make(map[string]bool)
	commonEdges := make(map[ctxgraph.Edge]bool, len(common.Edges))
	for _, node := range common.Nodes {
		commonNodes[node.ID] = true
	}
	for _, subgraph := range common.Subgraphs {
		commonSubgraphs[subgraph.ID] = true
	}
	for _, edge := range common.Edges {
		commonEdges[edge] = true
	}
	nodes, subgraphs := make(map[string]bool), make(map[string]bool)
	for _, node := range draft.Nodes {
		if !commonNodes[node.ID] {
			nodes[node.ID] = true
		}
	}
	for _, subgraph := range draft.Subgraphs {
		if !commonSubgraphs[subgraph.ID] {
			subgraphs[subgraph.ID] = true
		}
	}
	out := ctxgraph.Graph{Revision: draft.Revision}
	for _, edge := range draft.Edges {
		fromNode, isNode := strings.CutPrefix(edge.FromRef, "node:")
		fromSubgraph, isSubgraph := strings.CutPrefix(edge.FromRef, "subgraph:")
		if commonEdges[edge] && commonNodes[edge.ToNodeID] &&
			(!isNode || commonNodes[fromNode]) && (!isSubgraph || commonSubgraphs[fromSubgraph]) {
			continue
		}
		out.Edges = append(out.Edges, edge)
		nodes[edge.ToNodeID] = true
		if isNode {
			nodes[fromNode] = true
		}
		if isSubgraph {
			subgraphs[fromSubgraph] = true
		}
	}
	// Supersession chains are structural references, not a request to reorganize
	// other common nodes. Preserve the chain even when it points beyond this scope.
	for changed := true; changed; {
		changed = false
		for _, node := range draft.Nodes {
			if nodes[node.ID] && node.SupersededBy != "" && !nodes[node.SupersededBy] {
				nodes[node.SupersededBy] = true
				changed = true
			}
		}
	}
	for _, node := range draft.Nodes {
		if !nodes[node.ID] {
			continue
		}
		out.Nodes = append(out.Nodes, node)
		for _, id := range node.SubgraphIDs {
			subgraphs[id] = true
		}
	}
	for _, subgraph := range draft.Subgraphs {
		if subgraphs[subgraph.ID] {
			out.Subgraphs = append(out.Subgraphs, subgraph)
		}
	}
	return out
}

func inputMemoryQuery(request InputMemoryRequest, input ctxgraph.InputPartition, graph ctxgraph.Graph) string {
	readOnly := make([]string, 0)
	for _, node := range input.Common.Nodes {
		if slices.ContainsFunc(graph.Nodes, func(candidate ctxgraph.Node) bool { return candidate.ID == node.ID }) {
			readOnly = append(readOnly, ctxgraph.NodeRef(node.ID))
		}
	}
	for _, subgraph := range input.Common.Subgraphs {
		if slices.ContainsFunc(graph.Subgraphs, func(candidate ctxgraph.Subgraph) bool { return candidate.ID == subgraph.ID }) {
			readOnly = append(readOnly, ctxgraph.SubgraphRef(subgraph.ID))
		}
	}
	metadata := make([]ctxgraph.InputSource, 0, len(input.Differences))
	for _, source := range input.Differences {
		metadata = append(metadata, ctxgraph.InputSource{
			Ref: source.Ref, Graph: ctxgraph.Graph{Subgraphs: source.Graph.Subgraphs},
		})
	}
	sources, _ := json.Marshal(metadata)
	data, _ := json.Marshal(struct {
		ReadOnly []string       `json:"read_only_common_refs"`
		Graph    ctxgraph.Graph `json:"draft"`
	}{ReadOnly: readOnly, Graph: graph})
	return fmt.Sprintf("最终文件版本：%s\n最终文件证据（有界）：\n%s\n\n子图差异的原始来源（有界）：\n%s\n\n隔离记忆差异及必要的只读共同参照（有界）：\n%s",
		request.FilesRef,
		clipMiddle(request.Evidence, maxOrganizePromptBytes/4),
		clipMiddle(string(sources), maxOrganizePromptBytes/4),
		clipMiddle(string(data), maxOrganizePromptBytes/2),
	)
}

type boundedInputMemoryTool struct {
	inner       agenttool.Tool
	view        *inputMemoryView
	filesRef    string
	hasEvidence bool
}

func (t boundedInputMemoryTool) Definition() agenttool.Definition { return t.inner.Definition() }

func (t boundedInputMemoryTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if len(call.Arguments) > maxOrganizePromptBytes {
		return agenttool.Output{}, fmt.Errorf("input memory: tool arguments exceed %d bytes", maxOrganizePromptBytes)
	}
	if call.Name == agenttool.MemoryApplyName {
		var args struct {
			Ops []struct {
				Action string `json:"action"`
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"ops"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return agenttool.Output{}, err
		}
		var wire struct {
			Ops []map[string]json.RawMessage `json:"ops"`
		}
		var used map[string]bool
		nextID := 1
		for i, op := range args.Ops {
			if op.Action != ctxgraph.NodeChangeCreate && op.Action != ctxgraph.NodeChangeUpdate && op.Action != ctxgraph.NodeChangeStatus {
				continue
			}
			if op.Action != ctxgraph.NodeChangeStatus && op.Kind == ctxgraph.NodeKindDirective {
				directive := slices.ContainsFunc(t.view.graph.Nodes, func(node ctxgraph.Node) bool {
					return node.ID == op.ID && node.Kind == ctxgraph.NodeKindDirective
				})
				if !directive {
					return agenttool.Output{}, fmt.Errorf("input memory: organization cannot invent directives")
				}
			}
			// Earlier ops may have changed both kind and status. Only an explicit
			// kind written by this op qualifies its result without file evidence.
			accepts := op.Status == ctxgraph.NodeStatusAccepted ||
				(op.Status == "" && op.Action != ctxgraph.NodeChangeStatus)
			qualified := op.Action != ctxgraph.NodeChangeStatus &&
				(op.Kind == ctxgraph.NodeKindDirective || op.Kind == ctxgraph.NodeKindHypothesis)
			if accepts && !qualified &&
				(!t.hasEvidence || !strings.Contains(op.Reason, t.filesRef)) {
				return agenttool.Output{}, fmt.Errorf("input memory: accepted facts require evidence citing final files %q", t.filesRef)
			}
			if op.Action == ctxgraph.NodeChangeCreate && strings.TrimSpace(op.ID) == "" {
				if used == nil {
					// Allocation sees all reserved identities, while the model and
					// memory tools still see only the bounded difference graph.
					used = make(map[string]bool)
					for _, nodes := range [][]ctxgraph.Node{t.view.input.Common.Nodes, t.view.graph.Nodes} {
						for _, node := range nodes {
							used[node.ID] = true
						}
					}
					for _, candidate := range args.Ops {
						if candidate.Action == ctxgraph.NodeChangeCreate {
							used[strings.TrimSpace(candidate.ID)] = true
						}
					}
					if err := json.Unmarshal(call.Arguments, &wire); err != nil {
						return agenttool.Output{}, err
					}
				}
				for {
					id := fmt.Sprintf("mem-%d", nextID)
					nextID++
					if !used[id] {
						used[id] = true
						wire.Ops[i]["id"], _ = json.Marshal(id)
						break
					}
				}
			}
		}
		if used != nil {
			call.Arguments, _ = json.Marshal(wire)
		}
	}
	output, err := t.inner.Execute(ctx, call)
	output.Content = clipMiddle(output.Content, maxOrganizePromptBytes)
	return output, err
}
