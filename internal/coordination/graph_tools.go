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
		Description: "提交尚未执行切片的完整期望态，由图 diff 后改拓扑。roots 是根任务列表（按序号对齐，每项必须写 info）；spawns 是期望的 spawn/join 全集，每项带 info。成环、跨任务树或拆根会失败，图保持原样。任务执行期间不要改图。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"roots":{"type":"array","items":{"type":"object","properties":{"info":{"type":"string"}},"additionalProperties":false}},"spawns":{"type":"array","items":{"type":"object","properties":{"from":{"type":"string"},"join":{"type":"string"},"info":{"type":"string"}},"required":["from","join"],"additionalProperties":false}}},"additionalProperties":false}`),
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
