package coordination

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestJoinToolRequiresDispositionBeforeFinish(t *testing.T) {
	t.Parallel()

	join, root, child, _ := newJoinToolTest(t)
	_, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{
		task: child,
		out:  "candidate report",
	}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action:    "finish",
		SessionID: "join-1",
		Reason:    "done",
	})
	if err == nil || !strings.Contains(err.Error(), "undecided sources") {
		t.Fatalf("finish error = %v, want undecided sources", err)
	}
}

func TestJoinToolAppliesCandidateThenFinishes(t *testing.T) {
	t.Parallel()

	join, root, child, files := newJoinToolTest(t)
	if err := files.View(child.Env.ID).Write("result.txt", []byte("candidate")); err != nil {
		t.Fatal(err)
	}
	_, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{
		task: child,
		out:  "candidate report",
	}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action:    "apply",
		SessionID: "join-1",
		SourceID:  child.ID,
		Paths:     []string{"result.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := files.View(root.Env.ID).Read("result.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "candidate" {
		t.Fatalf("result.txt = %q, want candidate", got)
	}

	_, err = join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action:    "finish",
		SessionID: "join-1",
		Reason:    "accepted candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := join.requireFinished(root.Executor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-1", Reason: "idempotent replay",
	}); err != nil {
		t.Fatalf("finish replay: %v", err)
	}
	if _, err := files.View(child.Env.ID).Read("result.txt"); err == nil {
		t.Fatal("finished join retained candidate workspace")
	}
}

func TestJoinToolRequiresDispositionAfterPartialApply(t *testing.T) {
	t.Parallel()

	join, root, child, files := newJoinToolTest(t)
	for path, content := range map[string]string{
		"accepted.txt": "accepted",
		"rejected.txt": "rejected",
	} {
		if err := files.View(child.Env.ID).Write(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{
		task: child,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "apply", SessionID: "join-1", SourceID: child.ID,
		Paths: []string{"accepted.txt"},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-1", Reason: "done",
	})
	if err == nil || !strings.Contains(err.Error(), "undecided sources") {
		t.Fatalf("finish error = %v, want undecided sources", err)
	}

	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "discard", SessionID: "join-1", SourceIDs: []string{child.ID},
		Reason: "remaining paths are not needed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-1", Reason: "partial candidate accepted",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJoinToolDoesNotReleaseCandidateBeforeFinishIsDurable(t *testing.T) {
	t.Parallel()

	join, root, child, files := newJoinToolTest(t)
	if err := files.View(child.Env.ID).Write("result.txt", []byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if _, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{
		task: child,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "discard", SessionID: "join-1", SourceIDs: []string{child.ID}, Reason: "not selected",
	}); err != nil {
		t.Fatal(err)
	}

	failed := errors.New("save failed")
	progress := &failingProgressStore{ProgressStore: join.runner.progress, err: failed}
	join.runner.progress = progress
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-1", Reason: "done",
	}); !errors.Is(err, failed) {
		t.Fatalf("finish error = %v, want %v", err, failed)
	}
	if got, err := files.View(child.Env.ID).Read("result.txt"); err != nil || string(got) != "candidate" {
		t.Fatalf("candidate released before durable finish: body=%q err=%v", got, err)
	}
	if err := join.requireFinished(root.Executor.ID); err == nil {
		t.Fatal("failed finish remained completed in the in-memory session cache")
	}

	progress.err = nil
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-1", Reason: "retry",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.View(child.Env.ID).Read("result.txt"); err == nil {
		t.Fatal("durably finished join retained candidate workspace")
	}
}

func TestJoinToolRejectsAnotherRoleWorkspace(t *testing.T) {
	t.Parallel()

	join, root, child, _ := newJoinToolTest(t)
	_, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{task: child}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = join.execute(root.Executor.ID, root.Env.ID+":other", joinArgs{
		Action:    "inspect",
		SessionID: "join-1",
		SourceID:  child.ID,
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("inspect error = %v, want workspace ownership rejection", err)
	}
}

func TestJoinToolPaginatesCandidateFileContent(t *testing.T) {
	t.Parallel()

	join, root, child, files := newJoinToolTest(t)
	if err := files.View(child.Env.ID).Write("result.txt", []byte("候选文件内容")); err != nil {
		t.Fatal(err)
	}
	if _, err := join.open(root.ID, root.Executor.ID, root.Env.ID, "join-1", []joinedTask{{
		task: child,
		out:  "完整候选报告",
	}}); err != nil {
		t.Fatal(err)
	}
	summary, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "inspect", SessionID: "join-1", SourceID: child.ID, View: "summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	summaryPage := summary.(map[string]any)
	if _, exists := summaryPage["output"]; exists {
		t.Fatal("summary returned the unbounded output field")
	}
	if got := summaryPage["output_preview"]; got != "完整候选报告" {
		t.Fatalf("output_preview = %q", got)
	}
	result, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "inspect", SessionID: "join-1", SourceID: child.ID,
		View: "file", Path: "result.txt", Offset: 1, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	page, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("inspect result type = %T", result)
	}
	if got := page["content"]; got != "选文" {
		t.Fatalf("file page = %q, want %q", got, "选文")
	}
	if got := page["next_offset"]; got == nil || *got.(*int) != 3 {
		t.Fatalf("next_offset = %#v, want 3", got)
	}
}

func TestJoinToolCombinesSelectedPathsFromMultipleCandidates(t *testing.T) {
	t.Parallel()

	join, root, first, files := newJoinToolTest(t)
	second, err := join.graph.Spawn(root.Planner.ID, root.Executor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Fork(root.Env.ID, second.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(first.Env.ID).Write("first.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := files.View(second.Env.ID).Write("second.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	_, err = join.open(root.ID, root.Executor.ID, root.Env.ID, "join-many", []joinedTask{
		{task: first, out: "first report"},
		{task: second, out: "second report"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		source Task
		path   string
	}{
		{source: first, path: "first.txt"},
		{source: second, path: "second.txt"},
	} {
		if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
			Action: "apply", SessionID: "join-many", SourceID: item.source.ID, Paths: []string{item.path},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := join.execute(root.Executor.ID, root.Env.ID, joinArgs{
		Action: "finish", SessionID: "join-many", Reason: "combined both candidates",
	}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"first.txt": "first", "second.txt": "second"} {
		got, err := files.View(root.Env.ID).Read(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestJoinToolPlannerAndVerifierApplyOnlyToDisposableWorkspace(t *testing.T) {
	t.Parallel()

	for _, role := range []string{RolePlanner, RoleVerifier} {
		role := role
		t.Run(role, func(t *testing.T) {
			t.Parallel()

			join, root, child, files := newJoinToolTest(t)
			node := root.Planner
			if role == RoleVerifier {
				node = root.Verifier
			}
			targetID := root.Env.ID + ":" + role
			if err := files.Fork(root.Env.ID, targetID); err != nil {
				t.Fatal(err)
			}
			if err := files.View(child.Env.ID).Write("candidate.txt", []byte(role)); err != nil {
				t.Fatal(err)
			}
			_, err := join.open(root.ID, node.ID, targetID, "join-disposable", []joinedTask{{task: child}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := join.execute(node.ID, targetID, joinArgs{
				Action: "apply", SessionID: "join-disposable", SourceID: child.ID, All: true,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := files.View(root.Env.ID).Read("candidate.txt"); err == nil {
				t.Fatal("disposable role join leaked into persistent task workspace")
			}
			if got, err := files.View(targetID).Read("candidate.txt"); err != nil || string(got) != role {
				t.Fatalf("disposable candidate.txt = %q, %v", got, err)
			}
			if _, err := join.execute(node.ID, targetID, joinArgs{
				Action: "finish", SessionID: "join-disposable", Reason: "evidence consumed",
			}); err != nil {
				t.Fatal(err)
			}
			if err := files.Discard(targetID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newJoinToolTest(t *testing.T) (*joinCoordinator, Task, Task, *vfs.Store) {
	t.Helper()

	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	graph := newGraph()
	root := graph.AddTask()
	child, err := graph.Spawn(root.Planner.ID, root.Executor.ID)
	if err != nil {
		t.Fatal(err)
	}
	files := vfs.NewStore(base)
	if err := files.Fork(ManagerEnvID, root.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.Fork(root.Env.ID, child.Env.ID); err != nil {
		t.Fatal(err)
	}
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	join := &joinCoordinator{graph: graph, sessions: make(map[string][]JoinProgress)}
	run := &runner{
		graph:    graph,
		stores:   Stores{Memory: ctxgraph.NewStore(), Files: files},
		progress: progress,
		cancel:   func() {},
	}
	join.bind(run)
	t.Cleanup(func() { join.bind(nil) })
	return join, root, child, files
}

type failingProgressStore struct {
	ProgressStore
	err error
}

func (s *failingProgressStore) Save(taskID string, progress TaskProgress) error {
	if s.err != nil {
		return s.err
	}
	return s.ProgressStore.Save(taskID, progress)
}
