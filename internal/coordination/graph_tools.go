package coordination

import (
	"context"
	"encoding/json"
	"fmt"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const coordReplacePendingName = "coordination_replacePending"

type graphTool struct {
	name  string
	graph *Graph
}

var _ agenttool.Tool = graphTool{}

// GraphTools 把 ReplacePending 包装成 manager 专用工具。
func GraphTools(graph *Graph) []agenttool.Tool {
	return []agenttool.Tool{graphTool{name: coordReplacePendingName, graph: graph}}
}

// GraphToolMap 按名字取出 GraphTools，供 yaml NamedTools 安装。
func GraphToolMap(graph *Graph) map[string]agenttool.Tool {
	listed := GraphTools(graph)
	out := make(map[string]agenttool.Tool, len(listed))
	for _, tool := range listed {
		out[tool.Definition().Name] = tool
	}
	return out
}

func (t graphTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordReplacePendingName,
		Description: "提交协调图尚未开始部分的完整期望态。图执行中可以追加后续 root，或新增、删除、调整尚未开始节点上的辅助分支；已经开始的 task info 和节点关联边会被拒绝。roots 按序号对齐且只可追加；辅助任务只能写 spawns，不能写成额外 root。from/join 必须是当前图或上次工具结果中的完整节点 ID（如 task-1:planner），禁止占位符。新 root 需要 spawn 时分两次：先只提交 roots 取得真实 ID，再原样保留 roots 并提交完整 spawns。失败时图不变。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"roots":{"type":"array","minItems":1,"description":"全部根任务，必须保留现有顺序且只可追加；执行中的现有 root info 不可改。辅助分支不要放这里。","items":{"type":"object","properties":{"info":{"type":"string"}},"required":["info"],"additionalProperties":false}},"spawns":{"type":"array","description":"完整 spawn/join 期望集；执行中仅可改变尚未开始节点上的分支。新根先单独创建并从工具结果取得真实节点 ID。","items":{"type":"object","properties":{"from":{"type":"string","description":"尚未开始的真实来源节点 ID，例如 task-1:executor。"},"join":{"type":"string","description":"同一任务树中尚未开始的真实汇合节点 ID，例如 task-1:verifier。"},"info":{"type":"string"}},"required":["from","join","info"],"additionalProperties":false}}},"required":["roots"],"additionalProperties":false}`),
	}
}

func (t graphTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil graph", t.name)
	}
	var next PendingSubgraph
	if err := decodeGraphArgs(call.Arguments, &next); err != nil {
		return agenttool.Output{}, err
	}
	snap, err := t.graph.ReplacePending(ctx, next)
	if err != nil {
		return agenttool.Output{}, err
	}
	return encodeGraphJSON(snap)
}

func decodeGraphArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	return nil
}

func encodeGraphJSON(value any) (agenttool.Output, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode graph tool output: %w", err)
	}
	return agenttool.Output{Content: string(payload)}, nil
}
