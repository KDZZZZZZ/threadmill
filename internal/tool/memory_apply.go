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
	Name         string   `json:"name"`
	Summary      string   `json:"summary"`
	Admission    string   `json:"admission"`
	Scope        string   `json:"scope"`
	Reason       string   `json:"reason"`
}

// memoryApplyDescribeSubgraph 写子图说明；此时 ID 指目标子图而不是节点。
const memoryApplyDescribeSubgraph = "describe_subgraph"

// subgraphDescribe 是一条已通过校验的子图说明变更；空字段表示保留原值。
type subgraphDescribe struct {
	ID        string
	Name      string
	Summary   string
	Admission string
	Scope     string
}

type memoryApplyResult struct {
	Applied         int      `json:"applied"`
	CreatedIDs      []string `json:"created_ids,omitempty"`
	DescribedGraphs []string `json:"described_subgraph_ids,omitempty"`
}

func (t memoryApplyTool) Definition() Definition {
	return Definition{
		Name:        MemoryApplyName,
		Description: "对当前环境记忆图提交一批原子变更（create/update/status/delete/attach/detach/describe_subgraph），整批成功或整批不变；每条操作必须带 reason。describe_subgraph 写子图说明（name/summary/admission/scope），此时 id 指子图；子图必须已存在，system/package 子图由运行时管理不可写。命中保护层的节点操作会被拒绝：task-info-* 与 user/system 来源节点完全不可写；task-report-*/joined-report-* 只能改 status；directive 不可删除或改写 statement。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"ops":{"type":"array","minItems":1,"items":{"type":"object","properties":{"action":{"type":"string","enum":["create","update","status","delete","attach","detach","describe_subgraph"]},"id":{"type":"string","description":"目标节点 ID；create 时省略；describe_subgraph 时是目标子图 ID"},"kind":{"type":"string","enum":["directive","fact","hypothesis"],"description":"create 时必填"},"statement":{"type":"string","description":"create/update 时必填；update 为整条替换，不接受片段"},"status":{"type":"string","enum":["accepted","disputed","superseded","outdated"],"description":"status 操作必填"},"superseded_by":{"type":"string","description":"status=superseded 时的取代者节点 ID"},"subgraph_ids":{"type":"array","items":{"type":"string"},"description":"create/attach/detach 的目标子图"},"name":{"type":"string","description":"describe_subgraph：主题短名"},"summary":{"type":"string","description":"describe_subgraph：一句话说明这张子图回答什么"},"admission":{"type":"string","description":"describe_subgraph：准入内容——节点进入该子图必须满足的条件（纳入标准、证据要求、状态要求、排除项）"},"scope":{"type":"string","description":"describe_subgraph：适用范围——谁应在何时订阅，以及什么变化后应取消订阅"},"reason":{"type":"string","description":"本条操作理由，必填；引用证据或取代关系"}},"required":["action","reason"],"additionalProperties":false}}},"required":["ops"],"additionalProperties":false}`),
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
	changes, describes, err := memoryApplyOps(copy.Graph, args.Ops)
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
	described := make([]string, 0, len(describes))
	for _, describe := range describes {
		next = applySubgraphDescribe(next, describe)
		described = append(described, describe.ID)
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
	return marshalMemory(memoryApplyResult{
		Applied:         len(changes) + len(describes),
		CreatedIDs:      created,
		DescribedGraphs: described,
	})
}

// applySubgraphDescribe 用非空字段覆盖子图说明，省略字段保留原值。
// 子图存在性已在校验阶段确认；Graph.WithSubgraph 负责子图 revision 单调递增。
func applySubgraphDescribe(graph ctxgraph.Graph, describe subgraphDescribe) ctxgraph.Graph {
	subgraph, ok := graphSubgraph(graph, describe.ID)
	if !ok {
		return graph
	}
	if describe.Name != "" {
		subgraph.Name = describe.Name
	}
	if describe.Summary != "" {
		subgraph.Summary = describe.Summary
	}
	if describe.Admission != "" {
		subgraph.Admission = describe.Admission
	}
	if describe.Scope != "" {
		subgraph.Scope = describe.Scope
	}
	return graph.WithSubgraph(subgraph)
}

// memoryApplyOps 把模型提交的操作分成节点变更与子图说明两批，先做保护层与 reason 校验。
// 两批在同一次 commit 里落盘，任一条不合法则整批不变。
func memoryApplyOps(graph ctxgraph.Graph, ops []memoryApplyOp) ([]ctxgraph.NodeChange, []subgraphDescribe, error) {
	changes := make([]ctxgraph.NodeChange, 0, len(ops))
	describes := make([]subgraphDescribe, 0)
	for i, op := range ops {
		if strings.TrimSpace(op.Reason) == "" {
			return nil, nil, fmt.Errorf("op %d: reason is required", i+1)
		}
		if op.Action == memoryApplyDescribeSubgraph {
			describe, err := subgraphDescribeOp(graph, op)
			if err != nil {
				return nil, nil, fmt.Errorf("op %d (%s %q): %w", i+1, op.Action, op.ID, err)
			}
			describes = append(describes, describe)
			continue
		}
		node, found := graphNode(graph, op.ID)
		if found {
			if err := guardProtected(op, node); err != nil {
				return nil, nil, fmt.Errorf("op %d (%s %q): %w", i+1, op.Action, op.ID, err)
			}
		} else if op.ID != "" && op.Action != ctxgraph.NodeChangeCreate {
			return nil, nil, fmt.Errorf("op %d: node %q not found", i+1, op.ID)
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
	return changes, describes, nil
}

// subgraphDescribeOp 校验一条子图说明：子图必须已存在（不隐式创建），
// system/package 子图由运行时管理不可写，且至少要写一个字段。
func subgraphDescribeOp(graph ctxgraph.Graph, op memoryApplyOp) (subgraphDescribe, error) {
	id := strings.TrimSpace(op.ID)
	if id == "" {
		return subgraphDescribe{}, fmt.Errorf("describe_subgraph: id is required and must be a subgraph ID")
	}
	subgraph, ok := graphSubgraph(graph, id)
	if !ok {
		return subgraphDescribe{}, fmt.Errorf("subgraph %q not found; describe_subgraph does not create subgraphs", id)
	}
	switch subgraph.Kind {
	case ctxgraph.SubgraphKindSystem, ctxgraph.SubgraphKindPackage:
		return subgraphDescribe{}, fmt.Errorf("subgraph %q is runtime-managed (kind=%s) and cannot be described", id, subgraph.Kind)
	}
	describe := subgraphDescribe{
		ID:        id,
		Name:      strings.TrimSpace(op.Name),
		Summary:   strings.TrimSpace(op.Summary),
		Admission: strings.TrimSpace(op.Admission),
		Scope:     strings.TrimSpace(op.Scope),
	}
	if describe.Name == "" && describe.Summary == "" && describe.Admission == "" && describe.Scope == "" {
		return subgraphDescribe{}, fmt.Errorf("describe_subgraph: at least one of name/summary/admission/scope is required")
	}
	return describe, nil
}

func graphSubgraph(graph ctxgraph.Graph, id string) (ctxgraph.Subgraph, bool) {
	if id == "" {
		return ctxgraph.Subgraph{}, false
	}
	for _, subgraph := range graph.Subgraphs {
		if subgraph.ID == id {
			return subgraph, true
		}
	}
	return ctxgraph.Subgraph{}, false
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
