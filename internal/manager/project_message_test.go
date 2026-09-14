package manager

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestManagerAndRunningProjectAgentExchangeMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	replied := make(chan struct{})
	var once sync.Once
	file := loadRepoConfig(t)
	file.Agents.Manager = agent.FileAgent{SystemPrompt: "manager", Tools: []string{"coordination_orchestrate"}}
	file.Agents.Planner = agent.FileAgent{SystemPrompt: "planner"}
	file.Agents.Executor = agent.FileAgent{SystemPrompt: "executor", Tools: []string{"coordination_messageManager"}}
	file.Agents.Verifier = agent.FileAgent{SystemPrompt: "verifier"}
	executorCalls := 0
	observedReply := false
	llm := stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
		switch request.SystemPrompt {
		case "manager":
			query := lastUser(request.Messages)
			if strings.Contains(query, "[任务报告]") {
				return agent.AssistantMessage{Content: "results reviewed"}, nil
			}
			id, args := "create-project", `{"action":"replace_pending","tasks":[{"id":"project","info":"show working files","real_directory":true}]}`
			if strings.Contains(query, "progress-request") {
				id, args = "answer-project", `{"action":"message_task","task_id":"project","input":"manager-reply: 先检查配置"}`
			}
			for _, message := range request.Messages {
				if message.ToolResult != nil && message.ToolResult.CallID == id {
					if message.ToolResult.IsError {
						t.Errorf("manager tool failed: %+v", message.ToolResult)
					}
					if id == "answer-project" {
						once.Do(func() { close(replied) })
					}
					return agent.AssistantMessage{Content: "message handled"}, nil
				}
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{ID: id, Name: "coordination_orchestrate", Arguments: json.RawMessage(args)}}}, nil
		case "executor":
			executorCalls++
			if executorCalls == 1 {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{ID: "ask-manager", Name: "coordination_messageManager", Arguments: json.RawMessage(`{"message":"progress-request: 启动配置需要核对"}`)}}}, nil
			}
			select {
			case <-replied:
			case <-callCtx.Done():
				return agent.AssistantMessage{}, callCtx.Err()
			}
			for _, msg := range request.Messages {
				observedReply = observedReply || strings.Contains(msg.Content, "manager-reply: 先检查配置")
			}
			return agent.AssistantMessage{Content: "done"}, nil
		default:
			return agent.AssistantMessage{Content: "done"}, nil
		}
	})
	mgr, err := Open(ctx, Options{Root: t.TempDir(), File: file, Provider: llm})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	mgr.Send("show current project")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if !observedReply {
		t.Fatal("running project agent did not see manager reply")
	}
	if len(mgr.graph.Snapshot().ProjectMessages) != 2 {
		t.Fatalf("messages=%+v", mgr.graph.Snapshot().ProjectMessages)
	}
}
