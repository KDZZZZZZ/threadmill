package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const coordOrchestrateName = "coordination_orchestrate"
const coordPublishTaskName = "coordination_publishTask"

type orchestrateTool struct {
	graph *Graph
}

type orchestrateArgs struct {
	Action    string         `json:"action"`
	RequestID string         `json:"request_id,omitempty"`
	Roots     []PendingRoot  `json:"roots,omitempty"`
	Spawns    []PendingSpawn `json:"spawns,omitempty"`
}

type publishTaskTool struct {
	graph  *Graph
	stores Stores
}

var _ agenttool.Tool = orchestrateTool{}

// GraphTools wraps graph lifecycle operations as manager-only tools.
func GraphTools(graph *Graph, stores ...Stores) []agenttool.Tool {
	listed := []agenttool.Tool{orchestrateTool{graph: graph}}
	if len(stores) > 0 && stores[0].Files != nil {
		listed = append(listed, publishTaskTool{graph: graph, stores: stores[0]})
	}
	return listed
}

// GraphToolMap 按名字取出 GraphTools，供 yaml NamedTools 安装。
func GraphToolMap(graph *Graph, stores ...Stores) map[string]agenttool.Tool {
	listed := GraphTools(graph, stores...)
	out := make(map[string]agenttool.Tool, len(listed))
	for _, tool := range listed {
		out[tool.Definition().Name] = tool
	}
	return out
}

func (t publishTaskTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordPublishTaskName,
		Description: "把 manager 选定的已结束 task 文件快照渲染到真实项目路径，让用户看见当前进度。发布是阶段性检查点，不改变 verifier verdict 或 task outcome，也不消耗快照；随时可发，重新发布更早的检查点即可退回。结果里 changed 是用户实际看到的变化量。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1}},"required":["task_id"],"additionalProperties":false}`),
	}
}

func (t publishTaskTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil graph", coordPublishTaskName)
	}
	if t.stores.Files == nil {
		return agenttool.Output{}, fmt.Errorf("%s: file store unavailable", coordPublishTaskName)
	}
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	result, err := t.graph.publishTask(ctx, strings.TrimSpace(args.TaskID), t.stores)
	if err != nil {
		return agenttool.Output{}, err
	}
	return encodeGraphJSON(result)
}

// publishedPathLimit caps how many display paths a receipt names. A large
// checkpoint can touch thousands; the manager needs enough to describe what
// landed, not the whole list.
const publishedPathLimit = 60

type publishTaskResult struct {
	TaskID    string   `json:"task_id"`
	Outcome   string   `json:"outcome"`
	Published bool     `json:"published"`
	Changed   int      `json:"changed"`
	Added     []string `json:"added,omitempty"`
	Updated   []string `json:"updated,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
	Truncated bool     `json:"paths_truncated,omitempty"`
	Retained  string   `json:"retained_replaced,omitempty"`
}

type provideHelpResult struct {
	Snapshot
	Sources []helpSourceStatus `json:"sources"`
}

type helpSourceStatus struct {
	NodeID      string `json:"node_id"`
	OutputReady bool   `json:"output_ready"`
}

// publishTask renders one completed task's snapshot onto the display surface.
//
// Publication is a checkpoint the user can see, not a delivery claim, so it does
// not wait for a quiescent graph and does not consume anything: the snapshot,
// every sibling snapshot and every running environment survive it. That is what
// lets the manager show progress while work continues, and lets it render an
// earlier checkpoint again to go back.
func (g *Graph) publishTask(
	ctx context.Context,
	taskID string,
	stores Stores,
) (publishTaskResult, error) {
	if taskID == "" {
		return publishTaskResult{}, fmt.Errorf("%s: task_id is required", coordPublishTaskName)
	}
	// Publications serialise against each other but not against execution.
	g.publishMu.Lock()
	defer g.publishMu.Unlock()

	g.mu.Lock()
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		g.mu.Unlock()
		return publishTaskResult{}, fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if task.Outcome != OutcomeDone && task.Outcome != OutcomeFailed {
		g.mu.Unlock()
		return publishTaskResult{}, fmt.Errorf(
			"%s: task %q is not completed (outcome %q)",
			coordPublishTaskName,
			taskID,
			task.Outcome,
		)
	}
	g.mu.Unlock()

	selectedEnv := taskSnapshotEnvID(task)
	if err := stores.Files.Restore(selectedEnv); err != nil {
		if !errors.Is(err, vfs.ErrUnknownEnvironment) {
			return publishTaskResult{}, err
		}
		selectedEnv = task.Env.ID
		if err := stores.Files.Restore(selectedEnv); err != nil {
			return publishTaskResult{}, fmt.Errorf(
				"%s: restore task %q snapshot: %w",
				coordPublishTaskName,
				taskID,
				err,
			)
		}
	}
	if err := ctx.Err(); err != nil {
		return publishTaskResult{}, err
	}

	// The intent is recorded only once the snapshot is in hand, so a rejected
	// selection leaves no publication half-recorded in the graph.
	g.mu.Lock()
	before := g.stateLocked()
	g.publishingTaskID = taskID
	g.revision++
	intentErr := g.saveOrRestoreLocked(before)
	g.mu.Unlock()
	if intentErr != nil {
		return publishTaskResult{}, fmt.Errorf(
			"%s: record publication intent: %w",
			coordPublishTaskName,
			intentErr,
		)
	}

	receipt, err := stores.Files.Publish(selectedEnv)
	if err != nil {
		return publishTaskResult{}, fmt.Errorf(
			"%s: publish task %q: %w",
			coordPublishTaskName,
			taskID,
			err,
		)
	}

	g.mu.Lock()
	before = g.stateLocked()
	g.publishedTaskID = taskID
	g.publishingTaskID = ""
	g.revision++
	stateErr := g.saveOrRestoreLocked(before)
	g.mu.Unlock()
	if stateErr != nil {
		return publishTaskResult{}, fmt.Errorf(
			"%s: project updated; record publication for retry: %w",
			coordPublishTaskName,
			stateErr,
		)
	}
	return publishReceiptResult(taskID, task.Outcome, receipt), nil
}

func publishReceiptResult(
	taskID, outcome string,
	receipt vfs.PublishReceipt,
) publishTaskResult {
	result := publishTaskResult{
		TaskID:    taskID,
		Outcome:   outcome,
		Published: true,
		Changed:   receipt.Changed(),
		Retained:  receipt.Replaced,
	}
	budget := publishedPathLimit
	result.Added, budget = takePublishedPaths(receipt.Added, budget)
	result.Updated, budget = takePublishedPaths(receipt.Updated, budget)
	result.Deleted, budget = takePublishedPaths(receipt.Deleted, budget)
	result.Truncated = result.Changed > len(result.Added)+len(result.Updated)+len(result.Deleted)
	return result
}

func takePublishedPaths(paths []string, budget int) ([]string, int) {
	if budget <= 0 || len(paths) == 0 {
		return nil, budget
	}
	if len(paths) > budget {
		paths = paths[:budget]
	}
	return append([]string(nil), paths...), budget - len(paths)
}

func (t orchestrateTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordOrchestrateName,
		Description: "Manager 的唯一协调图编排入口。action=replace_pending 时提交尚未开始部分的完整 roots/spawns 期望态；action=provide_help 时用真实 request_id 接纳请求者给出的编排建议并物化 helper。已经开始的 task info 和节点关联边会被拒绝，失败时图不变。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["replace_pending","provide_help"]},"request_id":{"type":"string"},"roots":{"type":"array","minItems":1,"items":{"type":"object","properties":{"info":{"type":"string"},"run_policy":{"type":"string","enum":["enabled","held"]}},"required":["info"],"additionalProperties":false}},"spawns":{"type":"array","items":{"type":"object","properties":{"from":{"type":"string"},"join":{"type":"string"},"info":{"type":"string","minLength":1}},"required":["from","info"],"additionalProperties":false}}},"required":["action"],"additionalProperties":false}`),
	}
}

func (t orchestrateTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: nil graph", coordOrchestrateName)
	}
	var args orchestrateArgs
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	switch args.Action {
	case "replace_pending":
		if strings.TrimSpace(args.RequestID) != "" {
			return agenttool.Output{}, fmt.Errorf("%s: request_id is not valid for replace_pending", coordOrchestrateName)
		}
		snap, err := t.graph.ReplacePending(ctx, PendingSubgraph{Roots: args.Roots, Spawns: args.Spawns})
		if err != nil {
			return agenttool.Output{}, err
		}
		return encodeGraphJSON(snap)
	case "provide_help":
		if len(args.Roots) != 0 {
			return agenttool.Output{}, fmt.Errorf("%s: roots are not valid for provide_help", coordOrchestrateName)
		}
		requestID := strings.TrimSpace(args.RequestID)
		if requestID == "" {
			return agenttool.Output{}, fmt.Errorf("%s: request_id is required for provide_help", coordOrchestrateName)
		}
		for _, spawn := range args.Spawns {
			if strings.TrimSpace(spawn.Join) != "" {
				return agenttool.Output{}, fmt.Errorf("%s: join is assigned automatically for provide_help", coordOrchestrateName)
			}
		}
		t.graph.mu.Lock()
		help := t.graph.help
		t.graph.mu.Unlock()
		if help == nil {
			return agenttool.Output{}, fmt.Errorf("%s: help coordinator is unavailable", coordOrchestrateName)
		}
		result, err := help.provide(requestID, args.Spawns)
		if err != nil {
			return agenttool.Output{}, err
		}
		return encodeGraphJSON(result)
	default:
		return agenttool.Output{}, fmt.Errorf("%s: unsupported action %q", coordOrchestrateName, args.Action)
	}
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
