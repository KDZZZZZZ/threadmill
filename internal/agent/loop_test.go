package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

type modelFunc func(context.Context, Request) (AssistantMessage, error)

func (f modelFunc) Generate(ctx context.Context, request Request) (AssistantMessage, error) {
	return f(ctx, request)
}

func ignoreOrganize(next modelFunc) modelFunc {
	return func(ctx context.Context, request Request) (AssistantMessage, error) {
		if request.SystemPrompt == OrganizePrompt {
			return AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return next(ctx, request)
	}
}

func withOrganizeJSON(next modelFunc) modelFunc {
	return func(ctx context.Context, request Request) (AssistantMessage, error) {
		if request.SystemPrompt == OrganizePrompt {
			full := ""
			if len(request.Messages) > 0 {
				full = request.Messages[0].Content
			}
			body := full
			if i := strings.Index(body, "待整理对话："); i >= 0 {
				body = body[i:]
			}
			statement := "compacted"
			for _, needle := range []string{"remember blue", "old work", "start", "next"} {
				if strings.Contains(body, needle) {
					statement = needle
					break
				}
			}
			payload, err := json.Marshal(organizeOutput{
				Nodes: []organizeNode{{
					Kind:        ctxgraph.NodeKindFact,
					Statement:   statement,
					Status:      ctxgraph.NodeStatusAccepted,
					SubgraphIDs: subscribedFromOrganizePrompt(full),
				}},
			})
			if err != nil {
				return AssistantMessage{}, err
			}
			return AssistantMessage{Content: string(payload)}, nil
		}
		return next(ctx, request)
	}
}

func subscribedFromOrganizePrompt(content string) []string {
	const header = "当前订阅：\n"
	i := strings.Index(content, header)
	if i < 0 {
		return nil
	}
	rest := content[i+len(header):]
	if strings.HasPrefix(strings.TrimSpace(rest), "（无）") {
		return nil
	}
	ids := make([]string, 0)
	for _, line := range strings.Split(rest, "\n") {
		if line == "" || line == "可选归属子图：" {
			break
		}
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		id := strings.TrimPrefix(line, "- ")
		if j := strings.IndexByte(id, ' '); j >= 0 {
			id = id[:j]
		}
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

type testTool struct {
	definition agenttool.Definition
	execute    func(context.Context, agenttool.Call) (agenttool.Output, error)
}

func (t *testTool) Definition() agenttool.Definition {
	return t.definition
}

func (t *testTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	return t.execute(ctx, call)
}

func TestLoopRunsModelToolModelCycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requests := make([]Request, 0, 2)
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			return AssistantMessage{
				ModelData: json.RawMessage(`[{"type":"function_call","call_id":"call-1"}]`),
				Usage:     &Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "echo",
					Arguments: json.RawMessage(`{"text":"hello"}`),
				}},
			}, nil
		}
		return AssistantMessage{
			Content: "done",
			Usage:   &Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23},
		}, nil
	})

	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(_ context.Context, call agenttool.Call) (agenttool.Output, error) {
			if call.ID != "call-1" {
				t.Fatalf("tool call id = %q, want call-1", call.ID)
			}
			return agenttool.Output{Content: "hello"}, nil
		},
	}

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{echo},
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
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(requests) != 2 {
		t.Fatalf("model request count = %d, want 2", len(requests))
	}
	for i, request := range requests {
		if request.SystemPrompt != DefaultSystemPrompt {
			t.Fatalf("request %d system prompt = %q, want %q", i, request.SystemPrompt, DefaultSystemPrompt)
		}
	}
	firstMessages := requests[0].Messages
	if len(firstMessages) != 1 || firstMessages[0].Role != RoleUser || firstMessages[0].Content != "start" {
		t.Fatalf("first request messages = %#v", firstMessages)
	}

	messages := requests[1].Messages
	if len(messages) != 3 {
		t.Fatalf("second request message count = %d, want 3", len(messages))
	}
	wantModelData := json.RawMessage(`[{"type":"function_call","call_id":"call-1"}]`)
	if !reflect.DeepEqual(messages[1].ModelData, wantModelData) {
		t.Fatalf("assistant model data = %s, want %s", messages[1].ModelData, wantModelData)
	}
	wantFirstUsage := &Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	if !reflect.DeepEqual(messages[1].Usage, wantFirstUsage) {
		t.Fatalf("first assistant usage = %#v, want %#v", messages[1].Usage, wantFirstUsage)
	}

	result := messages[2].ToolResult
	if result == nil {
		t.Fatal("second request has no tool result")
	}
	want := &agenttool.Result{
		CallID:  "call-1",
		Name:    "echo",
		Content: "hello",
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("tool result = %#v, want %#v", result, want)
	}

	history := loop.Messages()
	wantSecondUsage := &Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}
	if !reflect.DeepEqual(history[3].Usage, wantSecondUsage) {
		t.Fatalf("second assistant usage = %#v, want %#v", history[3].Usage, wantSecondUsage)
	}
}

func TestLoopFeedsToolErrorsBackToModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var result *agenttool.Result
	calls := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "broken",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		result = request.Messages[len(request.Messages)-1].ToolResult
		return AssistantMessage{Content: "recovered"}, nil
	})

	broken := &testTool{
		definition: agenttool.Definition{
			Name:        "broken",
			Description: "Always fails",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			return agenttool.Output{}, errors.New("boom")
		},
	}

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{broken},
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
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if result == nil || !result.IsError || result.CallID != "call-1" || result.Content != "boom" {
		t.Fatalf("tool error result = %#v", result)
	}
}

func TestLoopPreemptsCurrentTurnAndProcessesQueuedMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	var (
		mu       sync.Mutex
		prompts  []string
		preempts int
	)
	model := modelFunc(func(ctx context.Context, request Request) (AssistantMessage, error) {
		prompt := lastUserContent(request.Messages)
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()

		if prompt == "first" {
			close(started)
			<-ctx.Done()
			return AssistantMessage{}, ctx.Err()
		}
		return AssistantMessage{Content: "second done"}, nil
	})

	loop, err := NewLoop(Config{
		Provider: model,
		Hooks: Hooks{
			OnPreempt: []TurnHook{
				func(context.Context, UserMessage) error {
					preempts++
					return nil
				},
			},
			AfterTurn: []AfterTurnHook{
				func(_ context.Context, message UserMessage, _ TurnResult) error {
					if message.Content == "second" {
						cancel()
					}
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Enqueue(UserMessage{Content: "first"})
	loop.Enqueue(UserMessage{Content: "second"})

	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()
	<-started
	if !loop.Preempt() {
		t.Fatal("Preempt() = false, want true")
	}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(prompts, []string{"first", "second"}) {
		t.Fatalf("processed prompts = %v", prompts)
	}
	if preempts != 1 {
		t.Fatalf("preempt hook count = %d, want 1", preempts)
	}
}

func TestLoopRejectsDuplicateToolCallIDsBeforeRecordingAssistantMessage(t *testing.T) {
	executed := false
	model := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		return AssistantMessage{ToolCalls: []agenttool.Call{
			{ID: "duplicate", Name: "echo", Arguments: json.RawMessage(`{}`)},
			{ID: "duplicate", Name: "echo", Arguments: json.RawMessage(`{}`)},
		}}, nil
	})
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			executed = true
			return agenttool.Output{}, nil
		},
	}

	loop, err := NewLoop(Config{Provider: model, Tools: []agenttool.Tool{echo}})
	if err != nil {
		t.Fatal(err)
	}
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(context.Background()); !errors.Is(err, agenttool.ErrInvalidCall) {
		t.Fatalf("Run() error = %v, want ErrInvalidCall", err)
	}
	if executed {
		t.Fatal("tool executed for duplicate call ids")
	}
	messages := loop.Messages()
	if len(messages) != 1 || messages[0].Role != RoleUser {
		t.Fatalf("messages = %#v, want only the user message", messages)
	}
}

func TestLoopRunsModelAndToolHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := []string{}
	modelCalls := 0
	model := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		modelCalls++
		if modelCalls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "echo",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		return AssistantMessage{Content: "done"}, nil
	})
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			events = append(events, "tool")
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{echo},
		Hooks: Hooks{
			BeforeModel: []BeforeModelHook{
				func(context.Context, Request) error {
					events = append(events, "before-model")
					return nil
				},
			},
			AfterModel: []AfterModelHook{
				func(_ context.Context, _ Request, _ AssistantMessage, err error) error {
					events = append(events, "after-model")
					return err
				},
			},
			BeforeTool: []BeforeToolHook{
				func(context.Context, agenttool.Call) error {
					events = append(events, "before-tool")
					return nil
				},
			},
			AfterTool: []AfterToolHook{
				func(_ context.Context, _ agenttool.Call, result agenttool.Result) error {
					if result.IsError {
						t.Fatalf("tool result unexpectedly failed: %#v", result)
					}
					events = append(events, "after-tool")
					return nil
				},
			},
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
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	want := []string{
		"before-model", "after-model",
		"before-tool", "tool", "after-tool",
		"before-model", "after-model",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("hook order = %v, want %v", events, want)
	}
}

func TestLoopBeforeToolHookCanBlockExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	executed := false
	var observed agenttool.Result
	modelCalls := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		modelCalls++
		if modelCalls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "dangerous",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		observed = *request.Messages[len(request.Messages)-1].ToolResult
		return AssistantMessage{Content: "done"}, nil
	})
	dangerous := &testTool{
		definition: agenttool.Definition{
			Name:        "dangerous",
			Description: "Run a dangerous action",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			executed = true
			return agenttool.Output{Content: "ran"}, nil
		},
	}

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{dangerous},
		Hooks: Hooks{
			BeforeTool: []BeforeToolHook{
				func(context.Context, agenttool.Call) error {
					return errors.New("blocked by policy")
				},
			},
			AfterTool: []AfterToolHook{
				func(_ context.Context, _ agenttool.Call, result agenttool.Result) error {
					if !result.IsError {
						t.Fatal("blocked tool result is not marked as an error")
					}
					return nil
				},
			},
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
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if executed {
		t.Fatal("tool executed after BeforeTool blocked it")
	}
	if !observed.IsError || observed.Content != "blocked by policy" {
		t.Fatalf("blocked result = %#v", observed)
	}
}

func lastUserContent(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

func TestLoopCompactsOverflowIntoSubscribedMemoryAndKeepsTail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var second Request
	calls := 0
	model := withOrganizeJSON(func(_ context.Context, request Request) (AssistantMessage, error) {
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
	mustAddMemoryHooks(t, loop)
	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
	})
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "start"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if !strings.Contains(second.SystemPrompt, "start") {
		t.Fatalf("second system prompt = %q, want compacted user memory", second.SystemPrompt)
	}
	if len(second.Messages) == 0 || second.Messages[0].Role != RoleAssistant {
		t.Fatalf("second messages = %#v, want the kept assistant tail", second.Messages)
	}
}

func TestLoopCommitsTailIntoMemoryWhenTurnEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var secondPrompt string
	turns := 0
	model := withOrganizeJSON(func(_ context.Context, request Request) (AssistantMessage, error) {
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
	mustAddMemoryHooks(t, loop)
	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
	})
	loop.SetSubscribedSubgraphs([]string{"sg-a"})
	loop.Enqueue(UserMessage{Content: "remember blue"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(loop.Messages()) != 0 {
		t.Fatalf("messages after last turn = %#v, want empty", loop.Messages())
	}
	if !strings.Contains(secondPrompt, "remember blue") {
		t.Fatalf("second system prompt = %q, want committed first-turn memory", secondPrompt)
	}

	graph := store.Load("env-1")
	nodes := graph.NodesInSubgraphs([]string{"sg-a"})
	if len(nodes) != 2 {
		t.Fatalf("memory nodes = %#v, want one node per turn", nodes)
	}
	if nodes[0].Statement != "remember blue" || nodes[1].Statement != "next" {
		t.Fatalf("turn memory = %#v", nodes)
	}
	sources := graph.SourceSubgraphsOf(nodes[0].ID)
	if len(sources) != 1 || sources[0] != "sg-a" {
		t.Fatalf("source subgraphs = %v", sources)
	}
	upstream := graph.UpstreamNodes(nodes[1].ID)
	if len(upstream) != 1 || upstream[0].ID != nodes[0].ID {
		t.Fatalf("assistant previous node = %#v", upstream)
	}
}

func TestRegularToolDoesNotReceiveTranscriptOrProvider(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var toolReceivedTranscript bool
	spyTool := &testTool{
		definition: agenttool.Definition{
			Name:        "spy",
			Description: "Spy tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
			_, ok := TranscriptFromContext(ctx)
			if ok {
				toolReceivedTranscript = true
			}
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	modelCalls := 0
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		modelCalls++
		if modelCalls == 1 {
			return AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "spy",
					Arguments: json.RawMessage(`{}`),
				}},
			}, nil
		}
		cancel()
		return AssistantMessage{Content: "done"}, nil
	})

	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{spyTool},
	})
	if err != nil {
		t.Fatal(err)
	}

	loop.Enqueue(UserMessage{Content: "run spy tool"})
	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	if toolReceivedTranscript {
		t.Fatal("regular tool received Transcript / Provider in context, want transcript isolated to hidden memory tools")
	}
}
