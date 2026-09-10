package coordination

import (
	"errors"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func newInputToolTest(t *testing.T, contents map[string]string) (*inputCoordinator, Task, InputProgress, *vfs.Store) {
	t.Helper()
	graph := New()
	task := graph.AddTask()
	files := vfs.NewStore(t.TempDir())
	if err := files.CreateEnvironment("", "base"); err != nil {
		t.Fatal(err)
	}
	if err := files.CreateEnvironment("base", "candidate"); err != nil {
		t.Fatal(err)
	}
	for path, content := range contents {
		if err := files.View("candidate").Write(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := files.PrepareInput("draft", []string{"base", "candidate"})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := InputProgress{
		ID: "input:" + task.Executor.ID, NodeID: task.Executor.ID, TargetID: "draft", Phase: "files", Paths: prepared.Paths,
		Sources: []InputSourceProgress{
			{ID: "base", EnvID: prepared.Candidates[0], FilesRef: "base", Discarded: true, Reason: "explicit test baseline"},
			{ID: "candidate", EnvID: prepared.Candidates[1], FilesRef: "candidate", Output: "unaccepted candidate report"},
		},
	}
	r := &runner{graph: graph, task: task, stores: Stores{Memory: ctxgraph.NewStore(), Files: files}, progress: progress, state: TaskProgress{Version: progressVersion, Inputs: []InputProgress{input}}}
	return &inputCoordinator{graph: graph, runner: r}, task, input, files
}

func TestInputRequiresDispositionBeforeFinish(t *testing.T) {
	input, task, batch, _ := newInputToolTest(t, map[string]string{"result.txt": "candidate"})
	_, err := input.execute(task.Executor.ID, batch.TargetID, inputArgs{Action: "finish", Reason: "done"})
	if err == nil || !strings.Contains(err.Error(), "undecided sources") {
		t.Fatalf("finish = %v", err)
	}
}

func TestInputPartialApplyNeedsRemainingDispositionAndRetainsSource(t *testing.T) {
	input, task, batch, files := newInputToolTest(t, map[string]string{"chosen.txt": "chosen", "rejected.txt": "rejected"})
	for _, action := range []inputArgs{
		{Action: "apply", SourceID: "candidate", Paths: []string{"chosen.txt"}},
		{Action: "discard", SourceID: "candidate", Reason: "remaining candidate changes rejected"},
		{Action: "finish", Reason: "file choices complete"},
	} {
		if _, err := input.execute(task.Executor.ID, batch.TargetID, action); err != nil {
			t.Fatal(err)
		}
	}
	got, err := files.View(batch.TargetID).Read("chosen.txt")
	if err != nil || string(got) != "chosen" {
		t.Fatalf("selected = %q, %v", got, err)
	}
	if _, err := files.View(batch.TargetID).Read("rejected.txt"); err == nil {
		t.Fatal("unselected path appeared")
	}
	if got, err := files.View("candidate").Read("rejected.txt"); err != nil || string(got) != "rejected" {
		t.Fatalf("immutable source consumed: %q, %v", got, err)
	}
	saved, _ := input.runner.inputState(batch.NodeID)
	if saved.Phase != "files_resolved" || saved.MemoryRef != "" {
		t.Fatalf("file finish made memory ready: %#v", saved)
	}
	if _, err := input.execute(task.Executor.ID, batch.TargetID, inputArgs{Action: "finish", SessionID: batch.ID, Reason: "replay"}); err != nil {
		t.Fatal(err)
	}
}

func TestInputFinishFailureDoesNotAdvanceOrRelease(t *testing.T) {
	input, task, batch, files := newInputToolTest(t, map[string]string{"result.txt": "candidate"})
	if _, err := input.execute(task.Executor.ID, batch.TargetID, inputArgs{Action: "discard", SourceID: "candidate", Reason: "rejected"}); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("persistence unavailable")
	input.runner.progress = &failingProgressStore{ProgressStore: input.runner.progress, err: failed}
	_, err := input.execute(task.Executor.ID, batch.TargetID, inputArgs{Action: "finish", Reason: "done"})
	if !errors.Is(err, failed) {
		t.Fatalf("finish = %v", err)
	}
	saved, _ := input.runner.inputState(batch.NodeID)
	if saved.Phase != "files" {
		t.Fatalf("failed transition leaked: %s", saved.Phase)
	}
	if _, err := files.View(batch.Sources[1].EnvID).Read("result.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestInputRejectsAnotherRoleAndWorkspace(t *testing.T) {
	input, task, batch, _ := newInputToolTest(t, map[string]string{"result.txt": "candidate"})
	for _, identity := range [][2]string{{task.Planner.ID, batch.TargetID}, {task.Executor.ID, "other"}} {
		if _, err := input.execute(identity[0], identity[1], inputArgs{Action: "inspect", SessionID: batch.ID, SourceID: "candidate"}); err == nil {
			t.Fatal("foreign role/workspace accessed candidates")
		}
	}
}

func TestInputCandidateFilePagination(t *testing.T) {
	input, task, batch, _ := newInputToolTest(t, map[string]string{"text.txt": "a中文bc"})
	result, err := input.execute(task.Executor.ID, batch.TargetID, inputArgs{Action: "inspect", SourceID: "candidate", View: "file", Path: "text.txt", Offset: 1, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	page := result.(map[string]any)
	if page["content"] != "中文" || *page["next_offset"].(*int) != 3 {
		t.Fatalf("page = %#v", page)
	}
}

type failingProgressStore struct {
	ProgressStore
	err error
}

func (s *failingProgressStore) Save(id string, progress TaskProgress) error {
	if s.err != nil {
		return s.err
	}
	return s.ProgressStore.Save(id, progress)
}
