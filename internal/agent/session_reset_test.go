package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// recordRequests 记下每次请求的消息快照，供断言历史是否被丢弃。
func recordRequests(answer string) (modelFunc, func() []Request) {
	var mu sync.Mutex
	var seen []Request
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		mu.Lock()
		seen = append(seen, request)
		mu.Unlock()
		return AssistantMessage{Content: answer}, nil
	})
	return model, func() []Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]Request(nil), seen...)
	}
}

func TestOrganizerDropsHistoryAndHandsOffTheGraphUnderPressure(t *testing.T) {
	resetDefaultStore(t)

	model, requests := recordRequests(strings.Repeat("整理结论。", 200))
	organizer, err := NewSubgraphOrganizer(Config{Provider: model, ContextWindow: 600})
	if err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	bindEnvGraph(t, organizer, store, "env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{
			ID:      "sg-q-3",
			Kind:    ctxgraph.SubgraphKindTask,
			Name:    "当前风险前沿",
			Scope:   "ARM/toolchain 腿未覆盖",
			Summary: "阻断交付的最小风险前沿",
		}},
	})
	organizer.noteSessionSubgraph("sg-q-3")

	if _, err := organizer.Ask(context.Background(), "第一次查询：清理 Autotools"); err != nil {
		t.Fatal(err)
	}
	if _, err := organizer.Ask(context.Background(), "第二次查询：版本与 ABI"); err != nil {
		t.Fatal(err)
	}

	seen := requests()
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want one per query", len(seen))
	}
	last := seen[1]
	for _, message := range last.Messages {
		if strings.Contains(message.Content, "第一次查询") {
			t.Fatalf("reset request still carries the earlier query: %#v", last.Messages)
		}
	}
	if len(last.Messages) == 0 || !strings.Contains(last.Messages[0].Content, "sg-q-3") {
		t.Fatalf("first message = %#v, want the graph handoff", last.Messages)
	}
	if !strings.Contains(last.Messages[0].Content, "ARM/toolchain 腿未覆盖") {
		t.Fatalf("handoff drops the subgraph scope: %q", last.Messages[0].Content)
	}
	if got := last.Messages[len(last.Messages)-1].Content; got != "第二次查询：版本与 ABI" {
		t.Fatalf("last message = %q, want the current query", got)
	}
}

func TestSessionResetKeepsCurrentTurnToolPairing(t *testing.T) {
	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.messages = []Message{
		{Role: RoleUser, Content: "旧查询"},
		{Role: RoleAssistant, Content: "旧回答"},
		{Role: RoleUser, Content: "当前查询"},
		{Role: RoleAssistant, ToolCalls: []agenttool.Call{{ID: "c-1", Name: memoryExpandToolName}}},
		{Role: RoleTool, ToolResult: &agenttool.Result{CallID: "c-1", Name: memoryExpandToolName}},
	}

	if err := loop.resetSession(); err != nil {
		t.Fatal(err)
	}

	got := loop.Messages()
	if len(got) != 4 {
		t.Fatalf("messages = %#v, want handoff plus the current turn", got)
	}
	if !strings.Contains(got[0].Content, "会话已重置") || got[1].Content != "当前查询" {
		t.Fatalf("messages = %#v, want the handoff before the current query", got)
	}
	if len(got[2].ToolCalls) != 1 || got[3].ToolResult == nil {
		t.Fatalf("messages = %#v, want the in-flight tool pair intact", got)
	}
}

func TestSessionResetStaysOffWithoutOrganizerRole(t *testing.T) {
	t.Parallel()

	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{}, nil
		}),
		ContextWindow: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.messages = []Message{
		{Role: RoleUser, Content: strings.Repeat("旧历史。", 100)},
		{Role: RoleUser, Content: "当前查询"},
	}
	if loop.shouldResetBeforeRequest(loop.assembleRequest()) {
		t.Fatal("a non-organizer loop must keep its history")
	}
}

func TestFileOverlayCanTurnOffOrganizerSessionReset(t *testing.T) {
	t.Parallel()

	model := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		return AssistantMessage{}, nil
	})
	agents := FileAgents{SubgraphOrganizer: FileAgent{ID: subgraphOrganizerID}}
	on, err := NewTeam(model, 1000, agents, nil, FileOverlay{})
	if err != nil {
		t.Fatal(err)
	}
	if !on.Organizer.sessionReset {
		t.Fatal("session reset must be on by default")
	}
	off, err := NewTeam(model, 1000, agents, nil, FileOverlay{NoSessionReset: true})
	if err != nil {
		t.Fatal(err)
	}
	if off.Organizer.sessionReset {
		t.Fatal("NoSessionReset must reach the organizer")
	}
	if on.Planner.sessionReset || on.Executor.sessionReset {
		t.Fatal("only the organizer resets its session; other roles compact")
	}
}

func TestSessionResetStopsWhenOnlyTheHandoffIsLeft(t *testing.T) {
	t.Parallel()

	loop, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{}, nil
		}),
		ContextWindow: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.sessionReset = true
	loop.messages = []Message{
		{Role: RoleUser, Content: "旧查询"},
		{Role: RoleUser, Content: strings.Repeat("当前查询很长。", 100)},
	}

	if !loop.shouldResetBeforeRequest(loop.assembleRequest()) {
		t.Fatal("history before the current turn must be droppable")
	}
	if err := loop.resetSession(); err != nil {
		t.Fatal(err)
	}
	if loop.shouldResetBeforeRequest(loop.assembleRequest()) {
		t.Fatal("a second reset would only rewrite the handoff")
	}
}
