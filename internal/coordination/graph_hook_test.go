package coordination

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

func TestInjectCoordinationGraphAppendsLatestSnapshot(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	graph.AddTask()

	hooks := InjectCoordinationGraph(graph)
	if len(hooks.AssembleRequest) != 1 {
		t.Fatalf("AssembleRequest hooks = %d, want 1", len(hooks.AssembleRequest))
	}

	got, err := hooks.AssembleRequest[0](context.Background(), agent.Request{SystemPrompt: "yaml manager"})
	if err != nil {
		t.Fatalf("hook error = %v", err)
	}
	if !strings.HasPrefix(got.SystemPrompt, "yaml manager") {
		t.Fatalf("system prompt = %q, want prefix yaml manager", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, "当前协调图：") {
		t.Fatalf("system prompt = %q, want injected graph label", got.SystemPrompt)
	}
	if !strings.Contains(got.SystemPrompt, `"ID":"task-1"`) {
		t.Fatalf("system prompt = %q, want latest task-1", got.SystemPrompt)
	}
}

func TestInjectCoordinationGraphNilGraph(t *testing.T) {
	t.Parallel()

	hooks := InjectCoordinationGraph(nil)
	_, err := hooks.AssembleRequest[0](context.Background(), agent.Request{SystemPrompt: "yaml manager"})
	if err == nil {
		t.Fatal("hook error = nil, want nil graph")
	}
}

func TestSnapshotAtUnknownRevision(t *testing.T) {
	t.Parallel()

	_, err := newGraph().SnapshotAt(context.Background(), 9)
	if !errors.Is(err, ErrUnknownRevision) {
		t.Fatalf("error = %v, want %v", err, ErrUnknownRevision)
	}
}
