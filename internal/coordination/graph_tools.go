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
)

const coordOrchestrateName = "coordination_orchestrate"

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

var _ agenttool.Tool = orchestrateTool{}

// GraphTools wraps graph lifecycle operations as manager-only tools.
func GraphTools(graph *Graph) []agenttool.Tool {
	return []agenttool.Tool{orchestrateTool{graph: graph}}
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

type provideHelpResult struct {
	Snapshot
	Sources []helpSourceStatus `json:"sources"`
}

type helpSourceStatus struct {
	NodeID      string `json:"node_id"`
	OutputReady bool   `json:"output_ready"`
}

func (t orchestrateTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        coordOrchestrateName,
		Description: "Manager 的协调图入口。replace_pending 提交尚未开始部分的完整 tasks/edges 期望态；provide_help 用 request_id 添加普通任务和边，只有显式指向 Resume 的边会阻塞请求者。continue_task 用 task_id 和 input 激活空闲的持久任务；close_task 结束指定持久任务。message_task 用 task_id 和 input 向正在运行的真实目录持有者发送消息，不启动已结束任务。已经冻结的节点输入不能修改，校验失败时图不变。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["replace_pending","provide_help","continue_task","close_task","message_task"]},"request_id":{"type":"string"},"task_id":{"type":"string"},"input":{"type":"string"},"tasks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","minLength":1},"info":{"type":"string"},"persistent":{"type":"boolean"},"real_directory":{"type":"boolean"},"run_policy":{"type":"string","enum":["enabled","held"]}},"required":["info"],"additionalProperties":false}},"edges":{"type":"array","items":{"type":"object","properties":{"from":{"type":"string","minLength":1},"to":{"type":"string","minLength":1}},"required":["from","to"],"additionalProperties":false}}},"required":["action"],"additionalProperties":false}`),
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
	case "message_task":
		if args.RequestID != "" || args.Tasks != nil || args.Edges != nil {
			return agenttool.Output{}, fmt.Errorf("%s: request_id, tasks and edges are not valid for message_task", coordOrchestrateName)
		}
		message, err := t.graph.MessageTask(strings.TrimSpace(args.TaskID), call.ID, args.Input)
		if err != nil {
			return agenttool.Output{}, err
		}
		return encodeGraphJSON(message)
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
