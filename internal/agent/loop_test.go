package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

type modelFunc func(context.Context, Request) (AssistantMessage, error)

func (f modelFunc) Generate(ctx context.Context, request Request) (AssistantMessage, error) {
	return f(ctx, request)
}

func TestIsRecoverableTurnErrorRejectsMixedFailures(t *testing.T) {
	t.Parallel()

	if !IsRecoverableTurnError(fmt.Errorf("wrapped: %w", ErrMaxSteps)) {
		t.Fatal("wrapped max steps should be recoverable")
	}
	if IsRecoverableTurnError(errors.Join(ErrMemoryFormat, errors.New("disk failed"))) {
		t.Fatal("memory format joined with a durable-state failure must not be recoverable")
	}
}

func isCompactRequest(request Request) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, "待整理对话：") {
			return true
		}
	}
	return false
}

func ignoreOrganize(next modelFunc) modelFunc {
	return func(ctx context.Context, request Request) (AssistantMessage, error) {
		if isCompactRequest(request) {
			return AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return next(ctx, request)
	}
}

func withOrganizeJSON(next modelFunc) modelFunc {
	return func(ctx context.Context, request Request) (AssistantMessage, error) {
		if isCompactRequest(request) {
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

func TestLoopMaterializesStateBlocksIntoAppendOnlyHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := "snapshot one"
	requests := make([]Request, 0, 3)
	model := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		requests = append(requests, request)
		if len(requests) < 3 {
			id := "call-" + string(rune('0'+len(requests)))
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        id,
				Name:      "advance",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}
		return AssistantMessage{Content: "done"}, nil
	})

	executions := 0
	advance := &testTool{
		definition: agenttool.Definition{
			Name:        "advance",
			Description: "Advance state",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			executions++
			if executions == 1 {
				state = "snapshot two"
			}
			return agenttool.Output{Content: "ok"}, nil
		},
	}
	loop, err := NewLoop(Config{
		Provider: model,
		Tools:    []agenttool.Tool{advance},
		Hooks: Hooks{
			AssembleRequest: []AssembleRequestHook{func(_ context.Context, request Request) (Request, error) {
				request.SetBlock("runtime", state)
				return request, nil
			}},
			AfterTurn: []AfterTurnHook{func(context.Context, UserMessage, TurnResult) error {
				cancel()
				return nil
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Enqueue(UserMessage{Content: "start"})
	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(requests))
	}
	for i, request := range requests {
		if len(request.StateBlocks) != 0 {
			t.Fatalf("request %d retained replaceable state blocks: %#v", i, request.StateBlocks)
		}
	}
	if !reflect.DeepEqual(requests[1].Messages[:len(requests[0].Messages)], requests[0].Messages) {
		t.Fatal("second request did not preserve the complete first-request prefix")
	}
	if !reflect.DeepEqual(requests[2].Messages[:len(requests[1].Messages)], requests[1].Messages) {
		t.Fatal("third request did not preserve the complete second-request prefix")
	}

	var snapshots []Message
	for _, message := range requests[2].Messages {
		if message.ContextBlockID == "runtime" {
			snapshots = append(snapshots, message)
		}
	}
	if len(snapshots) != 2 ||
		snapshots[0].Role != RoleUser || snapshots[1].Role != RoleUser ||
		!strings.Contains(snapshots[0].Content, "snapshot one") ||
		!strings.Contains(snapshots[1].Content, "snapshot two") {
		t.Fatalf("materialized snapshots = %#v, want one copy of each state", snapshots)
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

func TestLoopPublishesRuntimeEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []event.RuntimeEvent
	bus := event.NewBus(func(_ context.Context, ev event.RuntimeEvent) {
		got = append(got, ev)
	})
	modelCalls := 0
	model := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		modelCalls++
		if modelCalls == 1 {
			return AssistantMessage{
				Model: "stub-model",
				Usage: &Usage{TotalTokens: 9},
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "echo",
					Arguments: json.RawMessage(`{}`),
				}},
			}, nil
		}
		return AssistantMessage{Content: "done", Model: "stub-model"}, nil
	})
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo text",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			return agenttool.Output{Content: "ok"}, nil
		},
	}

	loop, err := NewLoop(Config{
		AgentID:  "planner",
		Provider: model,
		Tools:    []agenttool.Tool{echo},
		Events:   bus,
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

	want := []struct {
		kind  event.Kind
		phase event.Phase
		name  string
	}{
		{event.KindModel, event.PhaseStart, ""},
		{event.KindModel, event.PhaseEnd, "stub-model"},
		{event.KindTool, event.PhaseStart, "echo"},
		{event.KindTool, event.PhaseEnd, "echo"},
		{event.KindModel, event.PhaseStart, ""},
		{event.KindModel, event.PhaseEnd, "stub-model"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %#v, want %d", got, len(want))
	}
	for i, step := range want {
		if got[i].Kind != step.kind || got[i].Phase != step.phase || got[i].Name != step.name {
			t.Fatalf("event[%d] = kind=%s phase=%s name=%q, want %+v", i, got[i].Kind, got[i].Phase, got[i].Name, step)
		}
		if got[i].AgentID != "planner" {
			t.Fatalf("event[%d].AgentID = %q, want planner", i, got[i].AgentID)
		}
	}
	if got[1].ToolCalls != 1 || got[1].Tokens != 9 {
		t.Fatalf("first model end = %#v", got[1])
	}
	if got[2].CallID != "call-1" || got[3].CallID != "call-1" {
		t.Fatalf("tool call id = %q / %q", got[2].CallID, got[3].CallID)
	}
}

func TestLoopPublishesModelDelta(t *testing.T) {
	bus, got := recordingBus()
	loop, err := NewLoop(Config{
		AgentID: "manager",
		Provider: modelFunc(func(ctx context.Context, _ Request) (AssistantMessage, error) {
			sink := event.DeltaSink(ctx)
			if sink == nil {
				t.Fatal("missing delta sink")
			}
			sink("Hel")
			sink("lo")
			return AssistantMessage{Content: "Hello", Model: "stub-model"}, nil
		}),
		Events: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, ev := range *got {
		if ev.Phase == event.PhaseDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %q from %#v", deltas, *got)
	}
}

func TestLoopPublishesModelRetries(t *testing.T) {
	bus, got := recordingBus()
	loop, err := NewLoop(Config{
		AgentID: "manager",
		Provider: modelFunc(func(ctx context.Context, _ Request) (AssistantMessage, error) {
			sink := event.RetrySink(ctx)
			if sink == nil {
				t.Fatal("missing retry sink")
			}
			sink("transport")
			sink("stream_server_error")
			return AssistantMessage{Content: "done", Model: "stub-model"}, nil
		}),
		Events: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 4 || (*got)[0].Phase != event.PhaseStart ||
		(*got)[1].Phase != event.PhaseRetry || (*got)[1].Retries != 1 ||
		(*got)[1].RetryReason != "transport" ||
		(*got)[2].Phase != event.PhaseRetry || (*got)[2].Retries != 2 ||
		(*got)[2].RetryReason != "stream_server_error" ||
		(*got)[3].Phase != event.PhaseEnd || (*got)[3].Retries != 2 {
		t.Fatalf("events = %#v, want start, two retries, and end", *got)
	}
}

func TestLoopMarksBackgroundDeltasReplayableAndAllStreamsObservable(t *testing.T) {
	for _, test := range []struct {
		agentID string
		want    bool
	}{
		{agentID: "manager", want: false},
		{agentID: "planner", want: true},
	} {
		t.Run(test.agentID, func(t *testing.T) {
			loop, err := NewLoop(Config{
				AgentID: test.agentID,
				Provider: modelFunc(func(ctx context.Context, _ Request) (AssistantMessage, error) {
					if got := event.ReplayableDeltas(ctx); got != test.want {
						t.Fatalf("ReplayableDeltas() = %v, want %v", got, test.want)
					}
					if event.DeltaActivitySink(ctx) == nil {
						t.Fatal("missing delta activity sink")
					}
					return AssistantMessage{Content: "done"}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loop.Ask(context.Background(), "hi"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoopRecordsBackgroundTextActivityBeforeReplayableDelivery(t *testing.T) {
	collector := event.NewCollector()
	bus := event.NewBus(collector.Handle)
	loop, err := NewLoop(Config{
		AgentID: "planner",
		Provider: modelFunc(func(ctx context.Context, _ Request) (AssistantMessage, error) {
			activity := event.DeltaActivitySink(ctx)
			if activity == nil || !event.ReplayableDeltas(ctx) {
				t.Fatal("background stream is not observable and replayable")
			}
			activity(false)
			if got := collector.Snapshot().Model.TTFT.Count; got != 0 {
				t.Fatalf("non-text activity recorded TTFT count %d", got)
			}
			activity(true)
			if got := collector.Snapshot().Model.TTFT.Count; got != 1 {
				t.Fatalf("text activity TTFT count = %d, want 1 before delivery", got)
			}
			if sink := event.DeltaSink(ctx); sink != nil {
				sink("done")
			}
			return AssistantMessage{Content: "done"}, nil
		}),
		Events: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Ask(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCompactDoesNotPublishModelEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	bus, got := recordingBus()
	compactCalls := 0
	compactHadSink := false
	model := modelFunc(func(ctx context.Context, request Request) (AssistantMessage, error) {
		if isCompactRequest(request) {
			compactCalls++
			if event.DeltaSink(ctx) != nil {
				compactHadSink = true
			}
			if sink := event.RetrySink(ctx); sink != nil {
				sink("transport")
			}
			return AssistantMessage{
				Content: `{"nodes":[]}`,
				Usage:   &Usage{TotalTokens: 13},
			}, nil
		}
		if sink := event.DeltaSink(ctx); sink != nil {
			sink("hello")
		}
		return AssistantMessage{Content: "hello", Model: "chat"}, nil
	})

	loop, err := NewLoop(Config{
		AgentID:  "manager",
		Provider: model,
		Events:   bus,
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
	loop.Enqueue(UserMessage{Content: "hi"})

	if err := loop.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if compactCalls != 1 {
		t.Fatalf("compact calls = %d, want 1", compactCalls)
	}
	if compactHadSink {
		t.Fatal("compact generate received a delta sink")
	}

	var phases []event.Phase
	var deltas []string
	for _, ev := range *got {
		if ev.Kind != event.KindModel {
			continue
		}
		phases = append(phases, ev.Phase)
		if ev.Phase == event.PhaseDelta {
			deltas = append(deltas, ev.Delta)
		}
	}
	if strings.Join(deltas, "") != "hello" {
		t.Fatalf("deltas = %q from %#v", deltas, *got)
	}
	if len(phases) != 3 {
		t.Fatalf("model events = %#v, want one chat generate", *got)
	}
	var compactEvents []event.RuntimeEvent
	for _, ev := range *got {
		if ev.Kind == event.KindMemory && ev.Name == compactMemoryToolName {
			compactEvents = append(compactEvents, ev)
		}
	}
	if len(compactEvents) != 2 ||
		compactEvents[0].Phase != event.PhaseStart ||
		compactEvents[1].Phase != event.PhaseEnd {
		t.Fatalf("compact events = %#v, want start/end", compactEvents)
	}
	if compactEvents[1].Tokens != 13 || compactEvents[1].Retries != 1 {
		t.Fatalf("compact end = %#v, want hidden model cost", compactEvents[1])
	}
}

func TestLoopPublishesModelError(t *testing.T) {
	bus, got := recordingBus()
	loop, err := NewLoop(Config{
		AgentID: "planner",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{}, errors.New("provider down")
		}),
		Events: bus,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Ask(context.Background(), "start"); err == nil {
		t.Fatal("Ask() error = nil, want provider down")
	}
	if len(*got) != 2 {
		t.Fatalf("events = %#v, want start and end", *got)
	}
	if (*got)[0].Phase != event.PhaseStart || (*got)[1].Phase != event.PhaseEnd {
		t.Fatalf("phases = %s %s", (*got)[0].Phase, (*got)[1].Phase)
	}
	if (*got)[1].Err != "provider down" || !(*got)[1].IsError {
		t.Fatalf("end = %#v", (*got)[1])
	}
}

func recordingBus() (*event.Bus, *[]event.RuntimeEvent) {
	got := []event.RuntimeEvent{}
	bus := event.NewBus(func(_ context.Context, ev event.RuntimeEvent) {
		got = append(got, ev)
	})
	return bus, &got
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
	if !strings.Contains(blockText(second, "memory"), "start") {
		t.Fatalf("second memory block = %q, want compacted user memory", blockText(second, "memory"))
	}
	if len(second.Messages) == 0 || second.Messages[0].Role != RoleAssistant {
		t.Fatalf("second messages = %#v, want the kept assistant tail", second.Messages)
	}
}

func TestGenerateCompactsBeforeSendingOversizedRequest(t *testing.T) {
	resetDefaultStore(t)

	var normal Request
	sequence := make([]string, 0, 2)
	model := withOrganizeJSON(func(_ context.Context, request Request) (AssistantMessage, error) {
		sequence = append(sequence, "normal")
		normal = request
		return AssistantMessage{Content: "done"}, nil
	})
	wrapped := modelFunc(func(ctx context.Context, request Request) (AssistantMessage, error) {
		if isCompactRequest(request) {
			sequence = append(sequence, "compact")
		}
		return model.Generate(ctx, request)
	})
	loop, err := NewLoop(Config{
		AgentID:       "executor",
		Provider:      wrapped,
		ContextWindow: 600,
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
	loop.messages = []Message{
		{Role: RoleUser, Content: strings.Repeat("old", 600)},
		{Role: RoleAssistant, Content: strings.Repeat("tail", 300)},
		{Role: RoleUser, Content: "current request"},
	}

	if _, err := loop.generate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sequence, ","); got != "compact,normal" {
		t.Fatalf("model sequence = %q, want compact,normal", got)
	}
	if len(normal.Messages) != 3 || strings.Contains(normal.Messages[0].Content, "old") ||
		normal.Messages[2].ContextBlockID != "memory" {
		t.Fatalf("normal request messages = %#v, want compacted tail plus current memory", normal.Messages)
	}
	if got := estimateRequestTokens(normal); got >= softContextThreshold(loop.contextWindow) {
		t.Fatalf("normal request estimate = %d, want below soft threshold", got)
	}
}

func TestLoopCommitsTailIntoMemoryWhenTurnEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetDefaultStore(t)

	var secondMemory string
	turns := 0
	model := withOrganizeJSON(func(_ context.Context, request Request) (AssistantMessage, error) {
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
	if !strings.Contains(secondMemory, "remember blue") {
		t.Fatalf("second memory block = %q, want committed first-turn memory", secondMemory)
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
