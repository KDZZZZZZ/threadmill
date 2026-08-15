package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
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

	loop.SetContextGraph(ctxgraph.Graph{
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

	want := DefaultSystemPrompt + "\n\n记忆：\n- shared fact\n- only in a"
	if request.SystemPrompt != want {
		t.Fatalf("system prompt = %q, want %q", request.SystemPrompt, want)
	}
	if strings.Contains(request.SystemPrompt, "only in c") {
		t.Fatal("system prompt included a node from an unsubscribed subgraph")
	}
	if len(request.Messages) != 1 || request.Messages[0].Content != "start" {
		t.Fatalf("messages = %#v, want the original user message only", request.Messages)
	}
}

func TestAssembleRequestUsesCurrentSubgraphSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var loop *Loop
	prompts := make([]string, 0, 2)
	model := ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		prompts = append(prompts, request.SystemPrompt)
		if len(prompts) == 1 {
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

	loop.SetContextGraph(ctxgraph.Graph{
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
	if len(prompts) != 2 {
		t.Fatalf("model request count = %d, want 2", len(prompts))
	}

	wantFirst := DefaultSystemPrompt + "\n\n记忆：\n- memory a"
	if prompts[0] != wantFirst {
		t.Fatalf("first system prompt = %q, want %q", prompts[0], wantFirst)
	}
	wantSecond := DefaultSystemPrompt + "\n\n记忆：\n- memory b"
	if prompts[1] != wantSecond {
		t.Fatalf("second system prompt = %q, want %q", prompts[1], wantSecond)
	}
}

func TestAssembleRequestReadsLiveSubscribedSubgraphContent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	ctxgraph.Update(ctxgraph.Copy{
		Graph: ctxgraph.Graph{
			Nodes: []ctxgraph.Node{{
				ID:          "n1",
				Statement:   "old memory",
				SubgraphIDs: []string{"sg-a"},
			}},
		},
	})

	prompts := make([]string, 0, 2)
	model := ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		prompts = append(prompts, request.SystemPrompt)
		if len(prompts) == 1 {
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
			ctxgraph.Update(ctxgraph.Copy{
				Graph: ctxgraph.Graph{
					Nodes: []ctxgraph.Node{{
						ID:          "n1",
						Statement:   "new memory",
						SubgraphIDs: []string{"sg-a"},
					}},
				},
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
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("model request count = %d, want 2", len(prompts))
	}

	wantFirst := DefaultSystemPrompt + "\n\n记忆：\n- old memory"
	if prompts[0] != wantFirst {
		t.Fatalf("first system prompt = %q, want %q", prompts[0], wantFirst)
	}
	wantSecond := DefaultSystemPrompt + "\n\n记忆：\n- new memory"
	if prompts[1] != wantSecond {
		t.Fatalf("second system prompt = %q, want %q", prompts[1], wantSecond)
	}
}

func TestLoopsShareDefaultMemoryGraph(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var prompt string
	reader, err := NewLoop(Config{
		Provider: ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			prompt = request.SystemPrompt
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

	writer, err := NewLoop(Config{
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	writer.SetContextGraph(ctxgraph.Graph{
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

	want := DefaultSystemPrompt + "\n\n记忆：\n- shared fact"
	if prompt != want {
		t.Fatalf("reader system prompt = %q, want %q", prompt, want)
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

	loopA.SetContextGraph(ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Statement:   "only a",
			SubgraphIDs: []string{"sg-a"},
		}},
	})

	if loopA.graphCopy.AgentID != "agent-a" {
		t.Fatalf("loop A copy agent id = %q, want agent-a", loopA.graphCopy.AgentID)
	}
	if loopB.graphCopy.AgentID != "agent-b" {
		t.Fatalf("loop B copy agent id = %q, want agent-b", loopB.graphCopy.AgentID)
	}

	nodesA := loopA.ContextGraph().NodesInSubgraphs([]string{"sg-a"})
	if len(nodesA) != 1 || nodesA[0].Statement != "only a" {
		t.Fatalf("loop A copy = %#v, want only a", nodesA)
	}
	nodesB := loopB.ContextGraph().NodesInSubgraphs([]string{"sg-a"})
	if len(nodesB) != 0 {
		t.Fatalf("loop B copy = %#v, want empty unique copy", nodesB)
	}
}

func resetDefaultStore(t *testing.T) {
	t.Helper()
	ctxgraph.Update(ctxgraph.Copy{})
	t.Cleanup(func() {
		ctxgraph.Update(ctxgraph.Copy{})
	})
}
