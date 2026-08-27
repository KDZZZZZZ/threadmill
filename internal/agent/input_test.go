package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestAssembleRequestInjectsUnionMemoryFromSubscribedSubgraphs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var request Request
	model := ignoreOrganize(func(_ context.Context, got Request) (AssistantMessage, error) {
		request = got
		return AssistantMessage{Content: "done"}, nil
	})

	loop, err := NewLoop(Config{
		Provider: model,
		Hooks: Hooks{
			AfterTurn: []AfterTurnHook{
				func(context.Context, UserMessage, TurnResult) error {
					cancel()
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustAddMemoryHooks(t, loop)

	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{
			{
				ID:          "shared",
				Statement:   "shared fact",
				SubgraphIDs: []string{"sg-a", "sg-b"},
			},
			{
				ID:          "only-a",
				Statement:   "only in a",
				SubgraphIDs: []string{"sg-a"},
			},
			{
				ID:          "only-c",
				Statement:   "only in c",
				SubgraphIDs: []string{"sg-c"},
			},
			{
				ID:          "empty",
				Statement:   "",
				SubgraphIDs: []string{"sg-a"},
			},
		},
	})
	loop.SetSubscribedSubgraphs([]string{"sg-b", "sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	wantMemory := "记忆：\n- shared fact\n- only in a"
	if got := blockText(request, "memory"); got != wantMemory {
		t.Fatalf("memory block = %q, want %q", blockText(request, "memory"), wantMemory)
	}
	if request.SystemPrompt != DefaultSystemPrompt {
		t.Fatalf("system prompt = %q, want the bare default prompt", request.SystemPrompt)
	}
	if strings.Contains(request.WirePrompt(), "only in c") {
		t.Fatal("wire prompt included a node from an unsubscribed subgraph")
	}
	if len(request.Messages) != 1 || request.Messages[0].Content != "start" {
		t.Fatalf("messages = %#v, want the original user message only", request.Messages)
	}
}

// blockText 返回请求里指定 ID 的状态块文本；不存在时返回空串。
func blockText(request Request, id string) string {
	for _, block := range request.StateBlocks {
		if block.ID == id {
			return block.Text
		}
	}
	return ""
}

func TestAssembleRequestUsesCurrentSubgraphSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var loop *Loop
	memories := make([]string, 0, 2)
	model := ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		memories = append(memories, blockText(request, "memory"))
		if len(memories) == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "lookup",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		return AssistantMessage{Content: "done"}, nil
	})

	lookup := &testTool{
		definition: agenttool.Definition{
			Name:        "lookup",
			Description: "Look up a value",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			loop.SetSubscribedSubgraphs([]string{"sg-b"})
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	var err error
	loop, err = NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{lookup},
		Hooks: Hooks{
			AfterTurn: []AfterTurnHook{
				func(context.Context, UserMessage, TurnResult) error {
					cancel()
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustAddMemoryHooks(t, loop)

	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{
			{ID: "a", Statement: "memory a", SubgraphIDs: []string{"sg-a"}},
			{ID: "b", Statement: "memory b", SubgraphIDs: []string{"sg-b"}},
		},
	})
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(memories) != 2 {
		t.Fatalf("model request count = %d, want 2", len(memories))
	}

	wantFirst := "记忆：\n- memory a"
	if memories[0] != wantFirst {
		t.Fatalf("first memory block = %q, want %q", memories[0], wantFirst)
	}
	wantSecond := "记忆：\n- memory b"
	if memories[1] != wantSecond {
		t.Fatalf("second memory block = %q, want %q", memories[1], wantSecond)
	}
}

func TestAssembleRequestReadsLiveSubscribedSubgraphContent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	store := ctxgraph.NewStore()
	store.Save("env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Statement:   "old memory",
			SubgraphIDs: []string{"sg-a"},
		}},
	})

	memories := make([]string, 0, 2)
	model := ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		memories = append(memories, blockText(request, "memory"))
		if len(memories) == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "refresh",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		return AssistantMessage{Content: "done"}, nil
	})

	refresh := &testTool{
		definition: agenttool.Definition{
			Name:        "refresh",
			Description: "Refresh memory",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			store.Save("env-1", ctxgraph.Graph{
				Nodes: []ctxgraph.Node{{
					ID:          "n1",
					Statement:   "new memory",
					SubgraphIDs: []string{"sg-a"},
				}},
			})
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{refresh},
		Hooks: Hooks{
			AfterTurn: []AfterTurnHook{
				func(context.Context, UserMessage, TurnResult) error {
					cancel()
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustAddMemoryHooks(t, loop)
	if err := loop.Bind(env.Open("env-1", store.View("env-1"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(memories) != 2 {
		t.Fatalf("model request count = %d, want 2", len(memories))
	}

	wantFirst := "记忆：\n- old memory"
	if memories[0] != wantFirst {
		t.Fatalf("first memory block = %q, want %q", memories[0], wantFirst)
	}
	wantSecond := "记忆：\n- new memory"
	if memories[1] != wantSecond {
		t.Fatalf("second memory block = %q, want %q", memories[1], wantSecond)
	}
}

func TestLoopsShareDefaultMemoryGraph(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var memory string
	reader, err := NewLoop(Config{
		Provider: ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			memory = blockText(request, "memory")
			return AssistantMessage{Content: "done"}, nil
		}),
		Hooks: Hooks{
			AfterTurn: []AfterTurnHook{
				func(context.Context, UserMessage, TurnResult) error {
					cancel()
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustAddMemoryHooks(t, reader)

	store := ctxgraph.NewStore()
	bindEnvGraph(t, reader, store, "env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Statement:   "shared fact",
			SubgraphIDs: []string{"sg-a"},
		}},
	})
	reader.SetSubscribedSubgraphs([]string{"sg-a"})
	reader.Enqueue(UserMessage{Content: "start"})

	if err := reader.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	want := "记忆：\n- shared fact"
	if memory != want {
		t.Fatalf("reader memory block = %q, want %q", memory, want)
	}
}

func TestIndependentAgentCopyIsUnique(t *testing.T) {
	resetDefaultStore(t)

	loopA, err := NewLoop(Config{
		AgentID: "agent-a",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	loopB, err := NewLoop(Config{
		AgentID: "agent-b",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	store := ctxgraph.NewStore()
	graph := ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Statement:   "only a",
			SubgraphIDs: []string{"sg-a"},
		}},
	}
	bindEnvGraph(t, loopA, store, "env-a", graph)
	if err := loopB.Bind(env.Open("env-b", store.View("env-b"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	nodesA := store.Load("env-a").NodesInSubgraphs([]string{"sg-a"})
	if len(nodesA) != 1 || nodesA[0].Statement != "only a" {
		t.Fatalf("env-a = %#v, want only a", nodesA)
	}
	nodesB := store.Load("env-b").NodesInSubgraphs([]string{"sg-a"})
	if len(nodesB) != 0 {
		t.Fatalf("env-b = %#v, want empty unique copy", nodesB)
	}
}

func bindEnvGraph(t *testing.T, loop *Loop, store *ctxgraph.Store, envID string, graph ctxgraph.Graph) {
	t.Helper()
	store.Save(envID, graph)
	if err := loop.Bind(env.Open(envID, store.View(envID))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
}

func resetDefaultStore(t *testing.T) {
	t.Helper()
	ctxgraph.Update(ctxgraph.Copy{})
	t.Cleanup(func() {
		ctxgraph.Update(ctxgraph.Copy{})
	})
}
