package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// 第二次快照 revision 前进、executing 翻转，但图内容相同：投影字节必须逐字节一致，
	// 否则 manager 每轮都会因协调整理打掉整段历史前缀缓存。
	snap := graph.Snapshot()
	snap.Revision = snap.Revision + 7
	snap.Executing = true
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

// TestSnapshotPromptProjectionKeepsCreationOrder 锁定投影不得重排 tasks：
// ID 是 task-%d 的递增计数，任何字典序规范化都会在第 10 个 task 之后打乱创建顺序。
func TestSnapshotPromptProjectionKeepsCreationOrder(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	for range 12 {
		graph.AddTask()
	}
	payload, err := graph.Snapshot().PromptProjection()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Tasks []struct{ ID string } `json:"tasks"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 12 {
		t.Fatalf("tasks = %d, want 12", len(got.Tasks))
	}
	for i, task := range got.Tasks {
		want := fmt.Sprintf("task-%d", i+1)
		if task.ID != want {
			t.Fatalf("tasks[%d].ID = %q, want %q (创建顺序被重排)", i, task.ID, want)
		}
	}
}
