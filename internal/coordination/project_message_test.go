package coordination

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestProjectMessagesReachRunningRoleBeforeItFinishes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{Info: "debug", RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	task := snap.Tasks[0]
	files, err := vfs.NewPersistentStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	var notification string
	named := graph.HelpTools(func(text string) { notification = text })
	calls := 0
	provider := stubProvider(func(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if request.SystemPrompt != "executor" {
			return agent.AssistantMessage{Content: "done"}, nil
		}
		calls++
		if calls == 1 {
			data, _ := json.Marshal(map[string]string{"action": "message_task", "task_id": task.ID, "input": "请检查真实目录里的启动失败"})
			_, err := GraphTools(graph)[0].Execute(ctx, agenttool.Call{ID: "manager-note", Arguments: data})
			if err != nil {
				return agent.AssistantMessage{}, err
			}
			before := graph.Snapshot()
			if _, err := GraphTools(graph)[0].Execute(ctx, agenttool.Call{ID: "manager-note", Arguments: data}); err != nil {
				return agent.AssistantMessage{}, err
			}
			if !reflect.DeepEqual(before, graph.Snapshot()) {
				t.Error("retry duplicated manager message")
			}
			// This response was already in flight when the manager wrote.
			return agent.AssistantMessage{Content: "old final response"}, nil
		}
		found := false
		for _, msg := range request.Messages {
			found = found || strings.Contains(msg.Content, "请检查真实目录里的启动失败")
		}
		if !found {
			t.Error("manager message was not delivered to the running executor")
		}
		if calls == 2 {
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{ID: "reply", Name: "coordination_messageManager", Arguments: json.RawMessage(`{"message":"已经定位到启动配置"}`)}}}, nil
		}
		return agent.AssistantMessage{Content: "handled manager message"}, nil
	})
	assemble := Assemble(stores, provider, agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "planner"},
		Executor: agent.FileAgent{SystemPrompt: "executor", Tools: []string{"coordination_messageManager"}},
		Verifier: agent.FileAgent{SystemPrompt: "verifier"},
	}, nil, 0, nil, agent.FileOverlay{NamedTools: named})
	if _, err := graph.Run(t.Context(), task.ID, "", stores, assemble); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("executor model calls=%d, want 3", calls)
	}
	if !strings.Contains(notification, task.Executor.ID) || !strings.Contains(notification, "已经定位到启动配置") {
		t.Fatalf("manager notification=%q", notification)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := reopened.Snapshot().PromptProjection()
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"请检查真实目录里的启动失败", "已经定位到启动配置"} {
		if !strings.Contains(string(projection), text) {
			t.Fatalf("message missing after restart: %s", text)
		}
	}
	data, _ := json.Marshal(map[string]string{"action": "message_task", "task_id": task.ID, "input": "late"})
	if _, err := GraphTools(graph)[0].Execute(t.Context(), agenttool.Call{ID: "late", Arguments: data}); err == nil {
		t.Fatal("messaged a finished task")
	}
}

func TestProjectMessagesRejectUnauthorizedSendersAndPreserveGraphOnFailure(t *testing.T) {
	graph := New()
	ordinary := graph.AddTask()
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{ID: ordinary.ID}, {Info: "real", RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	owner := snap.ProjectTaskID
	tool := graph.HelpTools(func(string) {})[coordMessageManagerName]
	before := graph.Snapshot()
	for _, taskID := range []string{ordinary.ID, owner, "missing"} {
		if _, err := graph.MessageTask(taskID, "call", "hello"); err == nil {
			t.Fatalf("messaged unavailable task %q", taskID)
		}
	}
	for _, nodeID := range []string{ordinary.Executor.ID, "manager", "forged"} {
		if _, err := tool.Execute(agenttool.WithAgentID(t.Context(), nodeID), agenttool.Call{ID: "reply", Arguments: json.RawMessage(`{"message":"hello"}`)}); err == nil {
			t.Fatalf("accepted sender %q", nodeID)
		}
	}
	if !reflect.DeepEqual(graph.Snapshot(), before) {
		t.Fatal("rejected message changed graph")
	}
}
