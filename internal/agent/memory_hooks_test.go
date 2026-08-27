package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestHiddenCostProviderForwardsStreamActivity(t *testing.T) {
	t.Parallel()

	var got []bool
	provider := hiddenCostProvider{
		inner: modelFunc(func(ctx context.Context, _ Request) (AssistantMessage, error) {
			sink := event.DeltaActivitySink(ctx)
			if sink == nil {
				t.Fatal("missing stream activity sink")
			}
			sink(false)
			sink(true)
			return AssistantMessage{Usage: &Usage{
				InputTokens: 5, CachedTokens: 3, CacheWriteTokens: 1, TotalTokens: 7,
			}}, nil
		}),
		activity: func(text bool) { got = append(got, text) },
	}
	if _, err := provider.Generate(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("stream activity = %v, want [false true]", got)
	}
	if provider.tokens != 7 {
		t.Fatalf("tokens = %d, want 7", provider.tokens)
	}
	if provider.inputTokens != 5 || provider.cachedTokens != 3 || provider.cacheWriteTokens != 1 {
		t.Fatalf("cache usage = input %d cached %d write %d, want 5/3/1",
			provider.inputTokens, provider.cachedTokens, provider.cacheWriteTokens)
	}
}

func TestAssembleRequestOmitsSubscribedMemoryWithoutHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var prompt string
	var memory string
	loop, err := NewLoop(Config{
		Provider: ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			prompt = request.SystemPrompt
			memory = blockText(request, "memory")
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
	if memory != "" {
		t.Fatalf("memory block = %q, want none without the injection hook", memory)
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
	if strings.Contains(blockText(second, "memory"), "start") {
		t.Fatalf("second memory block = %q, want no compacted memory", blockText(second, "memory"))
	}
	if len(second.Messages) == 0 || second.Messages[0].Role != RoleUser {
		t.Fatalf("second messages = %#v, want the original user history", second.Messages)
	}
}

func TestLoopKeepsTailWithoutCommitHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var secondMemory string
	turns := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		if turns == 1 {
			secondMemory = blockText(request, "memory")
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
	if strings.Contains(secondMemory, "remember blue") {
		t.Fatalf("second memory block = %q, want no committed memory", secondMemory)
	}
}

func TestMemoryHooksRegistersHiddenTools(t *testing.T) {
	resetDefaultStore(t)
	loop, err := NewLoop(Config{
		Provider: ignoreOrganize(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.AddHooks(MemoryHooks(loop)); err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	if err := loop.Bind(env.Open("env-1", store.View("env-1"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	answer, err := loop.Ask(context.Background(), "start")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
}

func mustAddMemoryHooks(t *testing.T, loop *Loop) {
	t.Helper()
	if err := loop.AddHooks(MemoryHooks(loop)); err != nil {
		t.Fatal(err)
	}
}
