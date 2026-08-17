package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestAssembleRequestOmitsSubscribedMemoryWithoutHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var prompt string
	loop, err := NewLoop(Config{
		Provider: ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			prompt = request.SystemPrompt
			return AssistantMessage{Content: "done"}, nil
		}),
		Hooks: Hooks{AfterTurn: []AfterTurnHook{
			func(context.Context, UserMessage, TurnResult) error {
				cancel()
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if prompt != DefaultSystemPrompt {
		t.Fatalf("system prompt = %q, want the default prompt without memory", prompt)
	}
}

func TestLoopLeavesHistoryUncompactedWithoutOverflowHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var second Request
	calls := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{
				Content: "hello",
				Usage:   &Usage{TotalTokens: 50},
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "echo",
					Arguments: json.RawMessage(`{}`),
				}},
			}, nil
		}
		second = request
		return AssistantMessage{Content: "done", Usage: &Usage{TotalTokens: 4}}, nil
	})
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	loop, err := NewLoop(Config{
		Provider:      model,
		Tools:         []agenttool.Tool{echo},
		ContextWindow: 5,
		Hooks: Hooks{AfterTurn: []AfterTurnHook{
			func(context.Context, UserMessage, TurnResult) error {
				cancel()
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if strings.Contains(second.SystemPrompt, "start") {
		t.Fatalf("second system prompt = %q, want no compacted memory", second.SystemPrompt)
	}
	if len(second.Messages) == 0 || second.Messages[0].Role != RoleUser {
		t.Fatalf("second messages = %#v, want the original user history", second.Messages)
	}
}

func TestLoopKeepsTailWithoutCommitHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var secondPrompt string
	turns := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		if turns == 1 {
			secondPrompt = request.SystemPrompt
		}
		return AssistantMessage{Content: "ok", Usage: &Usage{TotalTokens: 3}}, nil
	})

	var loop *Loop
	loop, err := NewLoop(Config{
		Provider: model,
		Hooks: Hooks{AfterTurn: []AfterTurnHook{
			func(context.Context, UserMessage, TurnResult) error {
				turns++
				if turns == 1 {
					loop.Enqueue(UserMessage{Content: "next"})
					return nil
				}
				cancel()
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "remember blue"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(loop.Messages()) == 0 {
		t.Fatal("messages after last turn were committed, want the tail kept in history")
	}
	if strings.Contains(secondPrompt, "remember blue") {
		t.Fatalf("second system prompt = %q, want no committed memory", secondPrompt)
	}
}

func mustAddMemoryHooks(t *testing.T, loop *Loop) {
	t.Helper()
	if err := registerTools(loop, hiddenMemoryTools()); err != nil {
		t.Fatal(err)
	}
	if err := loop.AddHooks(MemoryHooks(loop)); err != nil {
		t.Fatal(err)
	}
}
