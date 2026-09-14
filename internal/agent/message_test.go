package agent

import (
	"context"
	"encoding/json"
	"testing"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestLoopRecordsPiFieldsOnUserAssistantAndTool(t *testing.T) {
	calls := 0
	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			calls++
			if calls == 1 {
				return AssistantMessage{
					Thinking: "need echo",
					API:      "openai-responses",
					Provider: "openai-responses",
					Model:    "gpt-5",
					ToolCalls: []agenttool.Call{{
						ID:        "call-1",
						Name:      "echo",
						Arguments: json.RawMessage(`{}`),
					}},
				}, nil
			}
			return AssistantMessage{Content: "done"}, nil
		}),
		Tools: []agenttool.Tool{&testTool{
			definition: agenttool.Definition{
				Name:        "echo",
				Description: "Echo",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
				return agenttool.Output{
					Content: "hello",
					Details: json.RawMessage(`{"echoed":true}`),
				}, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loop.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}

	got := loop.Messages()
	if len(got) != 4 {
		t.Fatalf("messages = %#v, want user, assistant, tool, assistant", got)
	}

	user, assistant, tool, final := got[0], got[1], got[2], got[3]
	if user.Role != RoleUser || user.Content != "start" || user.Timestamp == 0 {
		t.Fatalf("user = %#v, want timestamped start", user)
	}
	if assistant.Role != RoleAssistant ||
		assistant.Thinking != "need echo" ||
		assistant.StopReason != StopReasonToolUse ||
		assistant.API != "openai-responses" ||
		assistant.Model != "gpt-5" ||
		assistant.Timestamp == 0 {
		t.Fatalf("assistant = %#v, want Pi assistant fields", assistant)
	}
	if tool.Role != RoleTool ||
		tool.Timestamp == 0 ||
		tool.ToolResult == nil ||
		tool.ToolResult.Content != "hello" ||
		string(tool.ToolResult.Details) != `{"echoed":true}` {
		t.Fatalf("tool = %#v, want timestamped result details", tool)
	}
	if final.StopReason != StopReasonStop || final.Content != "done" || final.Timestamp == 0 {
		t.Fatalf("final = %#v, want stop reason stop", final)
	}
}
