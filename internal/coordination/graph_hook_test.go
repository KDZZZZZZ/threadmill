package coordination

import (
	"bytes"
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
	block := requestBlock(got, "coordination")
	if !strings.Contains(block, "当前协调图（JSON：") {
		t.Fatalf("coordination block = %q, want injected graph label", block)
	}
	if !strings.Contains(block, `"ID":"task-1"`) {
		t.Fatalf("coordination block = %q, want latest task-1", block)
	}
}

// requestBlock 返回请求里指定 ID 的状态块文本；不存在时返回空串。
func requestBlock(request agent.Request, id string) string {
	for _, block := range request.StateBlocks {
		if block.ID == id {
			return block.Text
		}
	}
	return ""
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

func TestSnapshotPromptProjectionIgnoresVolatileFields(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	graph.AddTask()
	graph.AddTask()
	first, err := graph.Snapshot().PromptProjection()
	if err != nil {
		t.Fatal(err)
	}

	// 第二次快照 revision 前进、executing 翻转，且 tasks/edges 顺序被打乱，但内容相同。
	snap := graph.Snapshot()
	snap.Revision = snap.Revision + 7
	snap.Executing = true
	for i, j := 0, len(snap.Tasks)-1; i < j; i, j = i+1, j-1 {
		snap.Tasks[i], snap.Tasks[j] = snap.Tasks[j], snap.Tasks[i]
	}
	for i, j := 0, len(snap.Edges)-1; i < j; i, j = i+1, j-1 {
		snap.Edges[i], snap.Edges[j] = snap.Edges[j], snap.Edges[i]
	}
	second, err := snap.PromptProjection()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("projection changed: %s vs %s", first, second)
	}
	if bytes.Contains(second, []byte("revision")) || bytes.Contains(second, []byte("executing")) {
		t.Fatalf("projection leaks volatile fields: %s", second)
	}
}
