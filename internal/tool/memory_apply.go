package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// MemoryApplyName 是批量整理记忆图的工具名；只应注册给记忆整理 Agent。
const MemoryApplyName = "memory_apply"

// 受保护的报告节点 ID 前缀：转录证据只可改 status，不可改写或删除。
var protectedReportPrefixes = []string{"task-report-", "joined-report-"}

// 契约层节点 ID 前缀：原始要求永不被整理掉。
var protectedDirectivePrefixes = []string{"task-info-"}

var protectedCreators = map[string]struct{}{"user": {}, "system": {}}

type memoryApplyTool struct {
	snapshot func() ctxgraph.Copy
	commit   func(ctxgraph.Copy) error
}

var (
	_ Tool      = memoryApplyTool{}
	_ EnvBinder = memoryApplyTool{}
)

type memoryApplyArgs struct {
	Ops []memoryApplyOp `json:"ops"`
}

type memoryApplyOp struct {
	Action       string   `json:"action"`
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Statement    string   `json:"statement"`
	Status       string   `json:"status"`
	SupersededBy string   `json:"superseded_by"`
	SubgraphIDs  []string `json:"subgraph_ids"`
	Reason       string   `json:"reason"`
}

type memoryApplyResult struct {
	Applied    int      `json:"applied"`
	CreatedIDs []string `json:"created_ids,omitempty"`
}

func (t memoryApplyTool) Definition() Definition {
	return Definition{
		Name:        MemoryApplyName,
		Description: "对当前环境记忆图提交一批原子变更（create/update/status/delete/attach/detach），整批成功或整批不变；每条操作必须带 reason。命中保护层的操作会被拒绝：task-info-* 与 user/system 来源节点完全不可写；task-report-*/joined-report-* 只能改 status；directive 不可删除或改写 statement。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"ops":{"type":"array","minItems":1,"items":{"type":"object","properties":{"action":{"type":"string","enum":["create","update","status","delete","attach","detach"]},"id":{"type":"string","description":"目标节点 ID；create 时省略"},"kind":{"type":"string","enum":["directive","fact","hypothesis"],"description":"create 时必填"},"statement":{"type":"string","description":"create/update 时必填；update 为整条替换，不接受片段"},"status":{"type":"string","enum":["accepted","disputed","superseded","outdated"],"description":"status 操作必填"},"superseded_by":{"type":"string","description":"status=superseded 时的取代者节点 ID"},"subgraph_ids":{"type":"array","items":{"type":"string"},"description":"create/attach/detach 的目标子图"},"reason":{"type":"string","description":"本条操作理由，必填；引用证据或取代关系"}},"required":["action","reason"],"additionalProperties":false}}},"required":["ops"],"additionalProperties":false}`),
	}
}

func (t memoryApplyTool) BindEnv(e env.Env) Tool {
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

func (t memoryApplyTool) Execute(ctx context.Context, call Call) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	if t.snapshot == nil || t.commit == nil {
		return Output{}, fmt.Errorf("%s: not bound to env", MemoryApplyName)
	}
	var args memoryApplyArgs
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return Output{}, fmt.Errorf("%s: decode arguments: %w", MemoryApplyName, err)
	}
	if len(args.Ops) == 0 {
		return Output{}, fmt.Errorf("%s: ops is required", MemoryApplyName)
	}

	copy := t.snapshot()
	changes, err := memoryApplyChanges(copy.Graph, args.Ops)
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", MemoryApplyName, err)
	}
	before := make(map[string]struct{}, len(copy.Graph.Nodes))
	for _, node := range copy.Graph.Nodes {
		before[node.ID] = struct{}{}
	}
	next, err := copy.Graph.WithNodeChanges(changes)
	if err != nil {
		return Output{}, fmt.Errorf("%s: %w", MemoryApplyName, err)
	}
	copy.Graph = next
	if err := t.commit(copy); err != nil {
		return Output{}, err
	}
	created := make([]string, 0)
	for _, node := range next.Nodes {
		if _, old := before[node.ID]; old {
			continue
		}
		created = append(created, node.ID)
	}
	return marshalMemory(memoryApplyResult{Applied: len(changes), CreatedIDs: created})
}

// memoryApplyChanges 把模型提交的操作翻译成图变更，先做保护层与 reason 校验。
func memoryApplyChanges(graph ctxgraph.Graph, ops []memoryApplyOp) ([]ctxgraph.NodeChange, error) {
	changes := make([]ctxgraph.NodeChange, 0, len(ops))
	for i, op := range ops {
		if strings.TrimSpace(op.Reason) == "" {
			return nil, fmt.Errorf("op %d: reason is required", i+1)
		}
		node, found := graphNode(graph, op.ID)
		if found {
			if err := guardProtected(op, node); err != nil {
				return nil, fmt.Errorf("op %d (%s %q): %w", i+1, op.Action, op.ID, err)
			}
		} else if op.ID != "" && op.Action != ctxgraph.NodeChangeCreate {
			return nil, fmt.Errorf("op %d: node %q not found", i+1, op.ID)
		}
		change := ctxgraph.NodeChange{
			Action:       op.Action,
			ID:           strings.TrimSpace(op.ID),
			Kind:         op.Kind,
			Statement:    op.Statement,
			Status:       op.Status,
			SupersededBy: strings.TrimSpace(op.SupersededBy),
			SubgraphIDs:  op.SubgraphIDs,
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func graphNode(graph ctxgraph.Graph, id string) (ctxgraph.Node, bool) {
	if id == "" {
		return ctxgraph.Node{}, false
	}
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return ctxgraph.Node{}, false
}

// guardProtected 按节点身份限制可执行的操作；契约层与报告层规则在这里强制。
func guardProtected(op memoryApplyOp, node ctxgraph.Node) error {
	creator := node.CreatorAgentID
	if _, locked := protectedCreators[creator]; locked || hasAnyPrefix(node.ID, protectedDirectivePrefixes) {
		return fmt.Errorf("node %q is contract memory (task info / user message) and cannot be modified", node.ID)
	}
	if hasAnyPrefix(node.ID, protectedReportPrefixes) {
		if op.Action != ctxgraph.NodeChangeStatus {
			return fmt.Errorf("node %q is a report transcript: only status changes are allowed", node.ID)
		}
		return nil
	}
	if node.Kind == ctxgraph.NodeKindDirective {
		switch op.Action {
		case ctxgraph.NodeChangeStatus:
			return nil
		case ctxgraph.NodeChangeUpdate, ctxgraph.NodeChangeDelete:
			return fmt.Errorf("node %q is a directive: only status changes are allowed", node.ID)
		}
	}
	if op.Action == ctxgraph.NodeChangeDelete &&
		node.Kind != ctxgraph.NodeKindFact && node.Kind != ctxgraph.NodeKindHypothesis {
		return fmt.Errorf("node %q: delete is only allowed for fact/hypothesis nodes", node.ID)
	}
	return nil
}

func hasAnyPrefix(id string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
