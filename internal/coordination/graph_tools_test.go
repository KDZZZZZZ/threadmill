package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestGraphToolsReplacePendingDiffsSpawns(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	tools := GraphTools(graph)

	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{"action":"replace_pending","roots":[{"info":"root"}]}`); err != nil {
		t.Fatalf("replacePending root: %v", err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}

	spawnArgs, err := json.Marshal(orchestrateArgs{
		Action: "replace_pending",
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Verifier.ID, Info: "help"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, string(spawnArgs)); err != nil {
		t.Fatalf("replacePending spawn: %v", err)
	}

	againArgs, err := json.Marshal(orchestrateArgs{
		Action: "replace_pending",
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Executor.ID, Join: root.Verifier.ID, Info: "help"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, string(againArgs)); err != nil {
		t.Fatalf("replacePending hot modify: %v", err)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("tasks = %d, want 2", graph.taskCount())
	}
}

func TestGraphToolsExposeSingleManagerOrchestrationTool(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	tools := GraphTools(graph)
	if len(tools) != 1 || tools[0].Definition().Name != coordOrchestrateName {
		t.Fatalf("manager graph tools = %v, want only %s", toolNames(tools), coordOrchestrateName)
	}

	for _, tool := range tools {
		name := tool.Definition().Name
		if name == "coordination_replacePending" || name == "coordination_provideHelp" {
			t.Fatalf("legacy manager orchestration tool %q is still exposed", name)
		}
	}
	if _, err := executeGraphTool(
		t,
		tools,
		"coordination_orchestrate",
		`{"action":"replace_pending","roots":[{"info":"root"}]}`,
	); err != nil {
		t.Fatalf("orchestrate replace_pending: %v", err)
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1", graph.taskCount())
	}
}

func toolNames(tools []agenttool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Definition().Name)
	}
	return names
}

func TestOrchestrateRejectsFieldsFromAnotherAction(t *testing.T) {
	t.Parallel()

	tools := GraphTools(newGraph())
	for _, test := range []struct {
		name string
		args string
		want string
	}{
		{
			name: "replace pending with request ID",
			args: `{"action":"replace_pending","request_id":"help/task-1:executor/call-1","roots":[{"info":"root"}]}`,
			want: "request_id is not valid",
		},
		{
			name: "provide help with roots",
			args: `{"action":"provide_help","request_id":"help/task-1:executor/call-1","roots":[{"info":"root"}],"spawns":[{"from":"task-1:planner","info":"help"}]}`,
			want: "roots are not valid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := executeGraphTool(t, tools, coordOrchestrateName, test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGraphToolsReplacePendingRejectsCycle(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	tools := GraphTools(graph)
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{"action":"replace_pending","roots":[{"info":"root"}]}`); err != nil {
		t.Fatal(err)
	}
	root, ok := graph.Task("task-1")
	if !ok {
		t.Fatal("root missing")
	}
	args, err := json.Marshal(orchestrateArgs{
		Action: "replace_pending",
		Roots:  []PendingRoot{{Info: "root"}},
		Spawns: []PendingSpawn{{From: root.Planner.ID, Join: root.Planner.ID, Info: "cycle"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executeGraphTool(t, tools, coordOrchestrateName, string(args))
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("error = %v, want %v", err, ErrJoinCycle)
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1", graph.taskCount())
	}
}

func TestGraphToolNilGraph(t *testing.T) {
	t.Parallel()

	_, err := orchestrateTool{}.Execute(context.Background(), agenttool.Call{
		ID:   "c1",
		Name: coordOrchestrateName,
	})
	if err == nil || !strings.Contains(err.Error(), "nil graph") {
		t.Fatalf("error = %v, want nil graph", err)
	}
}

func TestPublishTaskToolCommitsSelectedCompletedSnapshot(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	first := graph.AddTask()
	second := graph.AddTask()
	base := t.TempDir()
	files := vfs.NewStore(base)
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}

	if err := stores.Fork("", first.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(first.Env.ID).Write("choice.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(first.Env.ID, taskSnapshotEnvID(first)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := stores.Fork(first.Env.ID, second.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(second.Env.ID).Write("choice.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(second.Env.ID, taskSnapshotEnvID(second)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(second.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := files.Discard(first.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.Discard(second.Env.ID); err != nil {
		t.Fatal(err)
	}

	result, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+first.ID+`"}`,
	)
	if err != nil {
		t.Fatalf("publish selected task: %v", err)
	}
	if !strings.Contains(result.Content, `"published":true`) ||
		!strings.Contains(result.Content, `"choice.txt"`) {
		t.Fatalf("publish result = %s", result.Content)
	}
	got, err := os.ReadFile(filepath.Join(base, "choice.txt"))
	if err != nil || string(got) != "first" {
		t.Fatalf("published choice.txt = %q, %v, want first", got, err)
	}
	// Rendering a checkpoint consumes nothing: the sibling candidate is still
	// there to render instead.
	if _, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+second.ID+`"}`,
	); err != nil {
		t.Fatalf("render the other candidate: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "choice.txt")); err != nil || string(got) != "second" {
		t.Fatalf("re-rendered choice.txt = %q, %v, want second", got, err)
	}
}

func TestPublishTaskToolRecoversFailedLegacyWorkspace(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	task := graph.AddTask()
	base := t.TempDir()
	files := vfs.NewStore(base)
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Fork("", task.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(task.Env.ID).Write("recovered.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(task.ID, errors.New("legacy publish failed")); err != nil {
		t.Fatal(err)
	}

	result, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+task.ID+`"}`,
	)
	if err != nil {
		t.Fatalf("publish failed legacy task: %v", err)
	}
	if !strings.Contains(result.Content, `"outcome":"failed"`) {
		t.Fatalf("legacy publish result = %s", result.Content)
	}
	got, err := os.ReadFile(filepath.Join(base, "recovered.txt"))
	if err != nil || string(got) != "kept" {
		t.Fatalf("recovered file = %q, %v", got, err)
	}
}

func TestPublishTaskToolRejectsActiveTask(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	task := graph.AddTask()
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}

	_, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+task.ID+`"}`,
	)
	if err == nil || !strings.Contains(err.Error(), "not completed") {
		t.Fatalf("publish active task error = %v, want not completed", err)
	}
}

func TestPublishTaskToolDoesNotRecordIntentWithoutSnapshot(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	task := graph.AddTask()
	if err := graph.recordOutcome(task.ID, nil); err != nil {
		t.Fatal(err)
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}
	_, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+task.ID+`"}`,
	)
	if !errors.Is(err, vfs.ErrUnknownEnvironment) {
		t.Fatalf("publish missing snapshot error = %v", err)
	}
	if got := graph.Snapshot().PublishingTaskID; got != "" {
		t.Fatalf("publishing task = %q after preparation failure", got)
	}
}

func TestPublishTaskToolRendersWhileAnotherTaskRuns(t *testing.T) {
	t.Parallel()

	// Showing progress is the point, so a checkpoint is rendered as soon as it
	// exists. Waiting for the whole graph to fall quiet was a consequence of
	// publication writing into the read floor, and no longer applies.
	graph := newGraph()
	completed := graph.AddTask()
	graph.AddTask() // still active
	base := t.TempDir()
	files := vfs.NewStore(base)
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Fork("", completed.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(completed.Env.ID).Write("progress.txt", []byte("stage one")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(completed.Env.ID, taskSnapshotEnvID(completed)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(completed.ID, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+completed.ID+`"}`,
	); err != nil {
		t.Fatalf("publish with active task: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "progress.txt")); err != nil || string(got) != "stage one" {
		t.Fatalf("published progress.txt = %q, %v", got, err)
	}
}

func TestPublishTaskToolPersistsIdempotentReference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	graphPath := filepath.Join(root, "graph.json")
	graph, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "deliver"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks[0]
	base := filepath.Join(root, "project")
	state := filepath.Join(root, "vfs")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	files, err := vfs.NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Fork("", task.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(task.Env.ID).Write("delivered.txt", []byte("done")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(task.Env.ID, taskSnapshotEnvID(task)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(task.ID, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+task.ID+`"}`,
	); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if got := graph.Snapshot(); got.PublishedTaskID != task.ID || got.PublishingTaskID != "" {
		t.Fatalf("publication state = %+v", got)
	}

	restoredGraph, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	restoredFiles, err := vfs.NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeGraphTool(
		t,
		GraphTools(restoredGraph, Stores{Memory: ctxgraph.NewStore(), Files: restoredFiles}),
		coordPublishTaskName,
		`{"task_id":"`+task.ID+`"}`,
	)
	if err != nil {
		t.Fatalf("idempotent publish after restart: %v", err)
	}
	// Re-rendering the same checkpoint reconciles rather than short-circuits, so
	// it repairs a disturbed display and reports having changed nothing here.
	if !strings.Contains(result.Content, `"changed":0`) {
		t.Fatalf("idempotent result = %s", result.Content)
	}
	got, err := os.ReadFile(filepath.Join(base, "delivered.txt"))
	if err != nil || string(got) != "done" {
		t.Fatalf("delivered file = %q, %v", got, err)
	}
	next, err := restoredGraph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: ""}, {Info: "next"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.PublishedTaskID != task.ID {
		t.Fatalf("pending update lost published task: %+v", next)
	}
}

func TestPublishTaskToolPersistsIntentUntilSameTaskCanRetry(t *testing.T) {
	t.Parallel()

	graphPath := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "first"}, {Info: "second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, second := snapshot.Tasks[0], snapshot.Tasks[1]
	files := vfs.NewStore(filepath.Join(t.TempDir(), "missing-project"))
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Fork("", first.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(first.Env.ID).Write("threadmill-publish-intent", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(first.Env.ID, taskSnapshotEnvID(first)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := stores.Fork("", second.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(second.Env.ID, taskSnapshotEnvID(second)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(second.ID, nil); err != nil {
		t.Fatal(err)
	}

	_, err = executeGraphTool(
		t,
		GraphTools(graph, stores),
		coordPublishTaskName,
		`{"task_id":"`+first.ID+`"}`,
	)
	if err == nil {
		t.Fatal("publish missing project error = nil")
	}
	restored, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Snapshot(); got.PublishingTaskID != first.ID || got.PublishedTaskID != "" {
		t.Fatalf("restored intent = %+v", got)
	}
	// A recorded intent says which publication did not finish; it does not lock
	// the manager to that checkpoint. Checkpoints are versions to show, so
	// selecting a different one — including an earlier one — stays available.
	_, err = executeGraphTool(
		t,
		GraphTools(restored, stores),
		coordPublishTaskName,
		`{"task_id":"`+second.ID+`"}`,
	)
	if err != nil && strings.Contains(err.Error(), first.ID) {
		t.Fatalf("select another task was blocked by the intent on %s: %v", first.ID, err)
	}
}

func executeGraphTool(t *testing.T, tools []agenttool.Tool, name, arguments string) (agenttool.Output, error) {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name != name {
			continue
		}
		if err := tool.Definition().Validate(); err != nil {
			t.Fatalf("%s definition: %v", name, err)
		}
		return tool.Execute(context.Background(), agenttool.Call{
			ID:        "c-" + name,
			Name:      name,
			Arguments: json.RawMessage(arguments),
		})
	}
	t.Fatalf("tool %q not found", name)
	return agenttool.Output{}, nil
}
