package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestDropFromContextToolRemovesNodesFromHistoryNotGraph(t *testing.T) {
	resetDefaultStore(t)

	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.SetContextGraph(ctxgraph.Graph{
		Nodes: []ctxgraph.Node{
			{ID: "n-keep", Statement: "keep me"},
			{ID: "n-drop", Statement: "drop me"},
		},
	})

	payload := `{"before":[{"id":"n-drop","statement":"drop me"}],"after":[{"id":"n-keep","statement":"keep me"}]}`
	loop.appendMessage(Message{
		Role:    RoleTool,
		Content: payload,
		ToolResult: &agenttool.Result{
			CallID:  "call-1",
			Name:    "memory_neighbors",
			Content: payload,
		},
	})

	output, err := DropFromContextTool(loop).Execute(context.Background(), agenttool.Call{
		ID:        "call-drop",
		Name:      memoryDropFromContextToolName,
		Arguments: json.RawMessage(`{"node_ids":["n-drop"]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(output.Content, "n-drop") {
		t.Fatalf("output = %q, want dropped id listed", output.Content)
	}

	got := loop.Messages()
	if len(got) != 1 {
		t.Fatalf("messages = %#v, want the rewritten tool result", got)
	}
	if strings.Contains(got[0].Content, "n-drop") {
		t.Fatalf("content = %q, want n-drop removed", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "n-keep") {
		t.Fatalf("content = %q, want n-keep kept", got[0].Content)
	}
	if got[0].ToolResult == nil || strings.Contains(got[0].ToolResult.Content, "n-drop") {
		t.Fatalf("tool result = %#v, want n-drop removed", got[0].ToolResult)
	}

	graph := loop.ContextGraph()
	if len(graph.Nodes) != 2 {
		t.Fatalf("graph nodes = %#v, want both nodes still in memory", graph.Nodes)
	}
}

func TestRemindDropContextOnPressure(t *testing.T) {
	resetDefaultStore(t)

	var prompt string
	loop, err := NewLoop(Config{
		Provider: modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
			prompt = request.SystemPrompt
			return AssistantMessage{Content: "done"}, nil
		}),
		ContextWindow: 40,
		SystemPrompt:  "org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.AddHooks(Hooks{
		AssembleRequest: []AssembleRequestHook{RemindDropContextOnPressure(loop)},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := loop.Ask(context.Background(), "hi"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if strings.Contains(prompt, dropContextPressureReminder) {
		t.Fatalf("prompt = %q, want no reminder under the soft threshold", prompt)
	}

	if _, err := loop.Ask(context.Background(), strings.Repeat("n", 400)); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if !strings.Contains(prompt, dropContextPressureReminder) {
		t.Fatalf("prompt = %q, want the drop-context reminder", prompt)
	}
	if !strings.Contains(prompt, memoryDropFromContextToolName) {
		t.Fatalf("prompt = %q, want the tool name", prompt)
	}
}
