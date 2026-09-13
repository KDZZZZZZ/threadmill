package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Action    string        `json:"action"`
	RequestID string        `json:"request_id,omitempty"`
	TaskID    string        `json:"task_id,omitempty"`
	Input     string        `json:"input,omitempty"`
	Tasks     []PendingTask `json:"tasks,omitempty"`
	Edges     []Edge        `json:"edges,omitempty"`
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
		Description: "把 manager 选定任务的已提交文件快照渲染到真实项目路径，让用户看见当前进度。任务可已完成、失败或持久任务已空闲/关闭；新发布选择本轮最后已提交的角色输出，同一 task 的待重试发布复用原选定输出。发布不改变 task outcome，也不消耗快照。结果用 activation 和 node_id 标明实际选定出口，changed 是用户实际看到的变化量。",
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
	TaskID     string   `json:"task_id"`
	Activation uint64   `json:"activation"`
	NodeID     string   `json:"node_id"`
	Outcome    string   `json:"outcome"`
	Published  bool     `json:"published"`
	Changed    int      `json:"changed"`
	Added      []string `json:"added,omitempty"`
	Updated    []string `json:"updated,omitempty"`
	Deleted    []string `json:"deleted,omitempty"`
	Truncated  bool     `json:"paths_truncated,omitempty"`
	Retained   string   `json:"retained_replaced,omitempty"`
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
	selected, err := g.selectPublicationLocked(taskID)
	g.mu.Unlock()
	if err != nil {
		return publishTaskResult{}, err
	}
	if err := stores.Files.Restore(selected.FilesRef); err != nil {
		return publishTaskResult{}, fmt.Errorf("%s: restore task %q snapshot: %w", coordPublishTaskName, taskID, err)
	}
	if err := ctx.Err(); err != nil {
		return publishTaskResult{}, err
	}

	// The intent is recorded only once the snapshot is in hand, so a rejected
	// selection leaves no publication half-recorded in the graph.
	g.mu.Lock()
	before := g.stateLocked()
	g.publishing = selected
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

	receipt, err := stores.Files.Publish(selected.FilesRef)
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
	g.published = selected
	g.publishing = publicationState{}
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
	return publishReceiptResult(selected, receipt), nil
}

func (g *Graph) selectPublicationLocked(taskID string) (publicationState, error) {
	// A pending publication belongs to its selected output, even if the task
	// has continued to a later activation since that intent was recorded.
	if g.publishing.TaskID == taskID {
		return g.publishing, nil
	}
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		return publicationState{}, fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if task.Outcome != OutcomeDone && task.Outcome != OutcomeFailed && task.Outcome != OutcomeIdle && task.Outcome != OutcomeClosed {
		return publicationState{}, fmt.Errorf(
			"%s: task %q is not completed (outcome %q)", coordPublishTaskName, taskID, task.Outcome,
		)
	}
	sequence := task.Sequence()
	for i := len(sequence) - 1; i >= 0; i-- {
		if output, committed := g.outputs[sequence[i].ID]; committed && output.FilesRef != "" {
			return publicationState{
				TaskID: task.ID, Activation: task.Activation, NodeID: output.Node.ID,
				FilesRef: output.FilesRef, Outcome: task.Outcome,
			}, nil
		}
	}
	return publicationState{}, fmt.Errorf("%s: task %q has no committed file output", coordPublishTaskName, taskID)
}

func publishReceiptResult(
	selected publicationState,
	receipt vfs.PublishReceipt,
) publishTaskResult {
	result := publishTaskResult{
		TaskID:     selected.TaskID,
		Activation: selected.Activation,
		NodeID:     selected.NodeID,
		Outcome:    selected.Outcome,
		Published:  true,
		Changed:    receipt.Changed(),
		Retained:   receipt.Replaced,
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
		Description: "Manager 的协调图入口。replace_pending 提交尚未开始部分的完整 tasks/edges 期望态；provide_help 用 request_id 添加普通任务和边，只有显式指向 Resume 的边会阻塞请求者。continue_task 用 task_id 和 input 激活空闲的持久任务；close_task 结束指定持久任务。已经冻结的节点输入不能修改，校验失败时图不变。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["replace_pending","provide_help","continue_task","close_task"]},"request_id":{"type":"string"},"task_id":{"type":"string"},"input":{"type":"string"},"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"info":{"type":"string"},"persistent":{"type":"boolean"},"run_policy":{"type":"string","enum":["enabled","held"]}},"required":["info"],"additionalProperties":false}},"edges":{"type":"array","items":{"type":"object","properties":{"from":{"type":"string","minLength":1},"to":{"type":"string","minLength":1}},"required":["from","to"],"additionalProperties":false}}},"required":["action"],"additionalProperties":false}`),
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
		if args.TaskID != "" || args.Input != "" {
			return agenttool.Output{}, fmt.Errorf("%s: task_id and input are not valid for replace_pending", coordOrchestrateName)
		}
		snap, err := t.graph.ReplacePending(ctx, PendingSubgraph{Tasks: args.Tasks, Edges: args.Edges})
		if err != nil {
			return agenttool.Output{}, err
		}
		return encodeGraphReceipt(snap, nil)
	case "provide_help":
		if args.TaskID != "" || args.Input != "" {
			return agenttool.Output{}, fmt.Errorf("%s: task_id and input are not valid for provide_help", coordOrchestrateName)
		}
		requestID := strings.TrimSpace(args.RequestID)
		if requestID == "" {
			return agenttool.Output{}, fmt.Errorf("%s: request_id is required for provide_help", coordOrchestrateName)
		}
		t.graph.mu.Lock()
		help := t.graph.help
		t.graph.mu.Unlock()
		if help == nil {
			return agenttool.Output{}, fmt.Errorf("%s: help coordinator is unavailable", coordOrchestrateName)
		}
		result, err := help.provide(requestID, PendingSubgraph{Tasks: args.Tasks, Edges: args.Edges})
		if err != nil {
			return agenttool.Output{}, err
		}
		return encodeGraphReceipt(result.Snapshot, result.Sources)
	case "continue_task", "close_task":
		if args.RequestID != "" || args.Tasks != nil || args.Edges != nil {
			return agenttool.Output{}, fmt.Errorf("%s: request_id, tasks and edges are not valid for %s", coordOrchestrateName, args.Action)
		}
		taskID := strings.TrimSpace(args.TaskID)
		if taskID == "" {
			return agenttool.Output{}, fmt.Errorf("%s: task_id is required for %s", coordOrchestrateName, args.Action)
		}
		if args.Action == "continue_task" {
			task, err := t.graph.Continue(taskID, args.Input)
			if err != nil {
				return agenttool.Output{}, err
			}
			return encodeGraphJSON(task)
		}
		if args.Input != "" {
			return agenttool.Output{}, fmt.Errorf("%s: input is not valid for close_task", coordOrchestrateName)
		}
		if err := t.graph.CloseTask(taskID); err != nil {
			return agenttool.Output{}, err
		}
		task, _ := t.graph.Task(taskID)
		return encodeGraphJSON(task)
	default:
		return agenttool.Output{}, fmt.Errorf("%s: unsupported action %q", coordOrchestrateName, args.Action)
	}
}

func decodeGraphArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode arguments: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode arguments: expected one JSON value")
	}
	return nil
}

func encodeGraphReceipt(snapshot Snapshot, sources []helpSourceStatus) (agenttool.Output, error) {
	// Snapshot owns this output slice. Full reports remain in the canonical
	// graph injected before each Manager request; receipts need only references.
	for i := range snapshot.Outputs {
		snapshot.Outputs[i].Report = ""
	}
	return encodeGraphJSON(struct {
		Snapshot
		Sources               []helpSourceStatus `json:"sources,omitempty"`
		ReportsInCurrentGraph bool               `json:"reports_in_current_graph"`
	}{snapshot, sources, true})
}

func encodeGraphJSON(value any) (agenttool.Output, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("encode graph tool output: %w", err)
	}
	return agenttool.Output{Content: string(payload)}, nil
}
