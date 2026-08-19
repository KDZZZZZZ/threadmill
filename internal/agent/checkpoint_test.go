package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestAskDiscardsCheckpointWhenTurnCompletes(t *testing.T) {
	t.Parallel()

	store, err := NewDirCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const agentID = "completing-agent"
	var midOK bool
	loop, err := NewLoop(Config{
		AgentID:         agentID,
		CheckpointStore: store,
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
		Hooks: Hooks{
			AfterAssistant: []AfterAssistantHook{
				func(context.Context, AssistantMessage) error {
					_, midOK, err = store.Load(agentID)
					return err
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	answer, err := loop.Ask(context.Background(), "start")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if !midOK {
		t.Fatal("checkpoint missing during the in-progress turn")
	}
	if _, ok, err := store.Load(agentID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("checkpoint kept after the turn completed")
	}
}

func TestAskResumesIncompleteReactFromCheckpoint(t *testing.T) {
	t.Parallel()

	store, err := NewDirCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const agentID = "paused-agent"
	started := make(chan struct{})
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
			close(started)
			<-ctx.Done()
			return agenttool.Output{}, ctx.Err()
		},
	}
	first, err := NewLoop(Config{
		AgentID:         agentID,
		CheckpointStore: store,
		Tools:           []agenttool.Tool{echo},
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "call-1",
				Name:      "echo",
				Arguments: json.RawMessage(`{}`),
			}}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := first.Ask(ctx, "start"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask() error = %v, want context.Canceled", err)
	}
	if _, ok, err := store.Load(agentID); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("checkpoint discarded while the react was still in progress")
	}

	executed := false
	var resumeRequest Request
	second, err := NewLoop(Config{
		AgentID:         agentID,
		CheckpointStore: store,
		Tools: []agenttool.Tool{&testTool{
			definition: echo.definition,
			execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
				executed = true
				return agenttool.Output{Content: "should not run"}, nil
			},
		}},
		Provider: modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
			resumeRequest = request
			return AssistantMessage{Content: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	answer, err := second.Ask(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if executed {
		t.Fatal("resume re-executed a tool that already has a result")
	}
	if len(resumeRequest.Messages) < 3 {
		t.Fatalf("resume messages = %#v, want the paused react history", resumeRequest.Messages)
	}
	if resumeRequest.Messages[0].Content != "start" {
		t.Fatalf("resume first message = %#v, want the original user turn", resumeRequest.Messages[0])
	}
	if _, ok, err := store.Load(agentID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("checkpoint kept after the resumed turn completed")
	}
}

func TestAskResumesUnpairedToolCallsFromCheckpoint(t *testing.T) {
	t.Parallel()

	store, err := NewDirCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const agentID = "unpaired-agent"
	if err := store.Save(agentID, Checkpoint{
		Messages: []Message{
			{Role: RoleUser, Content: "start"},
			{
				Role: RoleAssistant,
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "echo",
					Arguments: json.RawMessage(`{}`),
				}},
			},
		},
		UsedToolCallIDs: []string{"call-1"},
	}); err != nil {
		t.Fatal(err)
	}

	executed := false
	var resumeRequest Request
	loop, err := NewLoop(Config{
		AgentID:         agentID,
		CheckpointStore: store,
		Tools: []agenttool.Tool{&testTool{
			definition: agenttool.Definition{
				Name:        "echo",
				Description: "Echo",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
				executed = true
				return agenttool.Output{Content: "hello"}, nil
			},
		}},
		Provider: modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
			resumeRequest = request
			return AssistantMessage{Content: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	answer, err := loop.Ask(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if !executed {
		t.Fatal("resume skipped unpaired tool calls")
	}
	if len(resumeRequest.Messages) < 3 || resumeRequest.Messages[2].Content != "hello" {
		t.Fatalf("resume request messages = %#v, want the tool result before the next generate", resumeRequest.Messages)
	}
	if _, ok, err := store.Load(agentID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("checkpoint kept after the resumed turn completed")
	}
}

type stickyDeleteStore struct {
	data      map[string]Checkpoint
	deleteErr error
}

func (s *stickyDeleteStore) Save(agentID string, checkpoint Checkpoint) error {
	if s.data == nil {
		s.data = make(map[string]Checkpoint)
	}
	s.data[agentID] = checkpoint
	return nil
}

func (s *stickyDeleteStore) Load(agentID string) (Checkpoint, bool, error) {
	checkpoint, ok := s.data[agentID]
	return checkpoint, ok, nil
}

func (s *stickyDeleteStore) Delete(agentID string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.data, agentID)
	return nil
}

func TestAskDoesNotRecommitWhenCheckpointDeleteFails(t *testing.T) {
	t.Parallel()

	deleteErr := errors.New("delete checkpoint")
	store := &stickyDeleteStore{deleteErr: deleteErr}
	commits := 0
	loop, err := NewLoop(Config{
		AgentID:         "commit-once",
		CheckpointStore: store,
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
		Hooks: Hooks{
			CommitTurn: []CommitTurnHook{
				func(context.Context) error {
					commits++
					return nil
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loop.Ask(context.Background(), "start"); !errors.Is(err, deleteErr) {
		t.Fatalf("Ask() error = %v, want %v", err, deleteErr)
	}
	if commits != 1 {
		t.Fatalf("commits after failed delete = %d, want 1", commits)
	}
	if _, ok, err := store.Load("commit-once"); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("checkpoint discarded after delete failed")
	}

	if _, err := loop.Ask(context.Background(), "ignored"); !errors.Is(err, deleteErr) {
		t.Fatalf("retry Ask() error = %v, want %v", err, deleteErr)
	}
	if commits != 1 {
		t.Fatalf("commits after retry = %d, want 1", commits)
	}

	store.deleteErr = nil
	answer, err := loop.Ask(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if commits != 1 {
		t.Fatalf("commits after successful discard = %d, want 1", commits)
	}
	if _, ok, err := store.Load("commit-once"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("checkpoint kept after discard succeeded")
	}
}
