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
	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{
			{ID: "n-keep", Statement: "keep me"},
			{ID: "n-drop", Statement: "drop me"},
		},
	})

	payload := `{"before":[{"id":"n-drop","statement":"drop me"}],"after":[{"id":"n-keep","statement":"keep me"}]}`
	if err := loop.appendMessage(Message{
		Role:    RoleTool,
		Content: payload,
		ToolResult: &agenttool.Result{
			CallID:  "call-1",
			Name:    "memory_neighbors",
			Content: payload,
		},
	}); err != nil {
		t.Fatal(err)
	}

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

	graph := store.Load("env-1")
	if len(graph.Nodes) != 2 {
		t.Fatalf("graph nodes = %#v, want both nodes still in memory", graph.Nodes)
	}
}

func TestRemindDropContextOnPressure(t *testing.T) {
	resetDefaultStore(t)

	var suffix string
	loop, err := NewLoop(Config{
		Provider: modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
			suffix = request.Suffix
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
	if strings.Contains(suffix, dropContextPressureReminder) {
		t.Fatalf("suffix = %q, want no reminder under the soft threshold", suffix)
	}

	if _, err := loop.Ask(context.Background(), strings.Repeat("n", 400)); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if !strings.Contains(suffix, dropContextPressureReminder) {
		t.Fatalf("suffix = %q, want the drop-context reminder", suffix)
	}
	if !strings.Contains(suffix, memoryDropFromContextToolName) {
		t.Fatalf("suffix = %q, want the tool name", suffix)
	}
}

func TestDropFromContextKeepsOldHistoryBytes(t *testing.T) {
	resetDefaultStore(t)

	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
		ContextWindow: 200, // 尾部可改写预算 = 100 token ≈ 400 字节
	})
	if err != nil {
		t.Fatal(err)
	}

	oldDetail := `[{"id":"n-drop","statement":"old drop me"}]` + strings.Repeat("x", 500)
	recentDetail := `[{"id":"n-drop","statement":"recent drop me"}]`
	if err := loop.appendMessage(Message{
		Role:    RoleUser,
		Content: oldDetail,
	}); err != nil {
		t.Fatal(err)
	}
	if err := loop.appendMessage(Message{
		Role:    RoleTool,
		Content: recentDetail,
		ToolResult: &agenttool.Result{
			CallID:  "call-2",
			Name:    "memory_neighbors",
			Content: recentDetail,
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := DropFromContextTool(loop).Execute(context.Background(), agenttool.Call{
		ID:        "call-drop",
		Name:      memoryDropFromContextToolName,
		Arguments: json.RawMessage(`{"node_ids":["n-drop"]}`),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	messages := loop.Messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if !strings.Contains(messages[0].Content, "n-drop") {
		t.Fatalf("old message rewritten: %q", messages[0].Content)
	}
	for _, part := range []string{messages[1].Content, messages[1].ToolResult.Content} {
		if strings.Contains(part, "n-drop") {
			t.Fatalf("recent message kept the dropped detail: %q", part)
		}
	}
}

// TestDropFromContextReportsProtectedPrefix 锁定工具如实报告：请求的节点全部落在
// 水位线之前的受保护前缀时，不能只回显请求 ID 让模型误以为压力已缓解。
func TestDropFromContextReportsProtectedPrefix(t *testing.T) {
	resetDefaultStore(t)

	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
		ContextWindow: 200, // 尾部可改写预算 = 100 token ≈ 400 字节
	})
	if err != nil {
		t.Fatal(err)
	}

	// 旧消息带目标节点，随后用一条超出改写预算的新消息把它挤出水位线。
	if err := loop.appendMessage(Message{
		Role:    RoleUser,
		Content: `[{"id":"n-old","statement":"protected"}]`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := loop.appendMessage(Message{
		Role:    RoleUser,
		Content: strings.Repeat("y", 600),
	}); err != nil {
		t.Fatal(err)
	}

	out, err := DropFromContextTool(loop).Execute(context.Background(), agenttool.Call{
		ID:        "call-drop",
		Name:      memoryDropFromContextToolName,
		Arguments: json.RawMessage(`{"node_ids":["n-old"]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Dropped           []string `json:"dropped"`
		RewrittenMessages int      `json:"rewritten_messages"`
		ProtectedMessages int      `json:"protected_messages"`
		Note              string   `json:"note"`
	}
	if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
		t.Fatal(err)
	}
	if got.RewrittenMessages != 0 {
		t.Fatalf("rewritten_messages = %d, want 0", got.RewrittenMessages)
	}
	if got.ProtectedMessages == 0 {
		t.Fatal("protected_messages = 0, want the old message counted as protected")
	}
	if got.Note == "" {
		t.Fatal("note is empty, want an explicit 未释放上下文 说明")
	}
	if !strings.Contains(loop.Messages()[0].Content, "n-old") {
		t.Fatal("protected message was rewritten")
	}
}
