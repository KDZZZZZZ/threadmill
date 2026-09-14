package coordination

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReplacePendingDefinesAndRemovesIndependentTasks(t *testing.T) {
	t.Parallel()
	graph := New()
	next := PendingSubgraph{
		Tasks: []PendingTask{{ID: "work", Info: "ship the change"}, {ID: "watch", Info: "keep observing", Persistent: true}},
		Edges: []Edge{{From: "work:1:planner", To: "watch:1:planner"}},
	}
	snap, err := graph.ReplacePending(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != 1 || len(snap.Tasks) != 2 {
		t.Fatalf("snapshot = %#v", snap)
	}
	watch, _ := graph.Task("watch")
	if !watch.Persistent {
		t.Fatal("persistent option lost")
	}
	for _, edge := range snap.Edges {
		if edge.From == watch.Verifier.ID {
			t.Fatalf("independent observer has an outgoing dependency: %#v", edge)
		}
	}
	before := graph.Snapshot()
	invalid := next
	invalid.Edges = []Edge{{From: "watch:1:verifier", To: "work:1:planner"}, {From: "work:1:verifier", To: "watch:1:planner"}}
	if _, err := graph.ReplacePending(context.Background(), invalid); err == nil {
		t.Fatal("accepted a cycle")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("cycle changed graph")
	}
	next.Tasks = next.Tasks[1:]
	next.Edges = nil
	snap, err = graph.ReplacePending(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != "watch" || len(snap.Nodes) != 3 || len(snap.Edges) != 2 {
		t.Fatalf("removed pending task remains in graph: %#v", snap)
	}
}

func TestReplacePendingRetainsCompletedHistoryAndAddsConsumers(t *testing.T) {
	t.Parallel()
	graph := New()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "source", Info: "produce a snapshot"}}})
	if err != nil {
		t.Fatal(err)
	}
	source := snap.Tasks[0]
	output := Output{Node: source.Verifier, FilesRef: "source-files", MemoryRef: "source-memory", Report: "ready"}
	if err := graph.commitOutput(output); err != nil {
		t.Fatal(err)
	}
	graph.tasks[0].Outcome = OutcomeDone
	next := PendingSubgraph{Tasks: []PendingTask{{ID: "consumer", Info: "consume immutable history"}}, Edges: []Edge{{From: source.Verifier.ID, To: "consumer:1:planner"}}}
	if _, err := graph.ReplacePending(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if got, ok := graph.Output(source.Verifier.ID); !ok || got != output {
		t.Fatalf("historical output lost: %#v", got)
	}
	if got, ok := graph.Task(source.ID); !ok || got.Outcome != OutcomeDone || got.Info != source.Info {
		t.Fatalf("completed task lost: %#v", got)
	}
	before := graph.Snapshot()
	next.Tasks = append(next.Tasks, PendingTask{ID: source.ID, Info: "rewritten past"})
	if _, err := graph.ReplacePending(context.Background(), next); err == nil {
		t.Fatal("changed completed task info")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("rejected completed edit mutated graph")
	}
}

func TestPendingEditsFreezeOnlyStartedInputsAcrossAllRunners(t *testing.T) {
	t.Parallel()
	graph := New()
	next := PendingSubgraph{Tasks: []PendingTask{{ID: "first", Info: "first"}, {ID: "second", Info: "second"}}}
	snap, err := graph.ReplacePending(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	first, second := snap.Tasks[0], snap.Tasks[1]
	for _, task := range snap.Tasks {
		graph.runners[task.ID] = &runner{task: task, nodeStarted: map[string]struct{}{task.Planner.ID: {}, task.Executor.ID: {}}}
	}
	next.Tasks = append(next.Tasks, PendingTask{ID: "later", Info: "use an existing output"})
	next.Edges = []Edge{{From: first.Planner.ID, To: "later:1:planner"}}
	if _, err := graph.ReplacePending(context.Background(), next); err != nil {
		t.Fatalf("adding outgoing consumer: %v", err)
	}
	for _, target := range []Node{first.Executor, second.Executor} {
		before := graph.Snapshot()
		invalid := next
		invalid.Edges = append(append([]Edge(nil), next.Edges...), Edge{From: "later:1:verifier", To: target.ID})
		if _, err := graph.ReplacePending(context.Background(), invalid); !errors.Is(err, ErrGraphBusy) {
			t.Fatalf("editing %s started input: %v", target.ID, err)
		}
		if !reflect.DeepEqual(before, graph.Snapshot()) {
			t.Fatal("started input edit changed graph")
		}
		if err := graph.Connect("later:1:verifier", target.ID); !errors.Is(err, ErrGraphBusy) {
			t.Fatalf("Connect editing %s started input: %v", target.ID, err)
		}
	}
	invalid := next
	invalid.Tasks = append([]PendingTask(nil), next.Tasks...)
	invalid.Tasks[1].Info = "different request"
	if _, err := graph.ReplacePending(context.Background(), invalid); !errors.Is(err, ErrGraphBusy) {
		t.Fatalf("editing running task info: %v", err)
	}
}

func TestTaskSinkReceivesExistingAndUpdatedTaskInfo(t *testing.T) {
	graph := New()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{ID: "work", Info: "initial"}},
	}); err != nil {
		t.Fatal(err)
	}

	var got []Task
	if err := graph.SetTaskSink(func(tasks []Task) error {
		got = append(got, tasks...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{ID: "work", Info: "updated"}},
	}); err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0].Info != "initial" || got[1].Info != "updated" {
		t.Fatalf("sink tasks = %#v, want initial then updated task info", got)
	}
}

func TestReplacePendingRetriesFailedTaskSinkWithoutDuplicatingTasks(t *testing.T) {
	path := t.TempDir() + "/graph.json"
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	sinkErr := errors.New("sink failed")
	fail := true
	if err := graph.SetTaskSink(func([]Task) error {
		if fail {
			return sinkErr
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := PendingSubgraph{Tasks: []PendingTask{{Info: "work"}}}
	if _, err := graph.ReplacePending(context.Background(), want); !errors.Is(err, sinkErr) {
		t.Fatalf("first ReplacePending() error = %v, want sink error", err)
	}
	if got := graph.taskCount(); got != 0 {
		t.Fatalf("tasks after failed sink = %d, want graph unchanged", got)
	}
	restored, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.taskCount(); got != 0 {
		t.Fatalf("persisted tasks after failed sink = %d, want graph unchanged", got)
	}

	fail = false
	if _, err := graph.ReplacePending(context.Background(), want); err != nil {
		t.Fatalf("retry ReplacePending() error = %v", err)
	}
	if got := graph.taskCount(); got != 1 {
		t.Fatalf("tasks after retry = %d, want no duplicate task", got)
	}
}

func TestPendingKeepsStartedTaskInfoAfterRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "work", Info: "original goal"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.commitOutput(Output{Node: snap.Tasks[0].Planner, FilesRef: "plan-files", MemoryRef: "plan-memory"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	before := reopened.Snapshot()
	if _, err := reopened.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "work", Info: "replace goal after planner"}}}); !errors.Is(err, ErrGraphBusy) {
		t.Fatalf("editing started goal after restart: %v", err)
	}
	if !reflect.DeepEqual(before, reopened.Snapshot()) {
		t.Fatal("failed edit changed graph")
	}
}

func TestPendingCannotLeaveDanglingHelpHistory(t *testing.T) {
	t.Parallel()
	graph := New()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "request", Info: "request"}, {ID: "help", Info: "help"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := snap.Tasks[0]
	pause := Node{ID: request.Planner.ID + ":pause", TaskID: request.ID, Role: RolePlanner}
	resume := Node{ID: request.Planner.ID + ":resume", TaskID: request.ID, Role: RolePlanner}
	for _, node := range []Node{pause, resume} {
		if err := graph.addCheckpointNode(node); err != nil {
			t.Fatal(err)
		}
	}
	graph.helps = []helpState{{ID: "request-1", CallID: "call-1", NodeID: request.Planner.ID, PauseID: pause.ID, ResumeID: resume.ID, TaskIDs: []string{"help"}, Configured: true}}
	before := graph.Snapshot()
	if _, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: request.ID, Info: request.Info}}}); err == nil {
		t.Fatal("removed a task still referenced by help history")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("rejected removal changed graph")
	}
}

func TestPendingRunPolicyAndInputValidation(t *testing.T) {
	t.Parallel()
	graph := New()
	want := PendingSubgraph{Tasks: []PendingTask{{ID: "work", Info: "do work", RunPolicy: RunPolicyHeld}}}
	snap, err := graph.ReplacePending(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Tasks[0].RunPolicy != RunPolicyHeld {
		t.Fatal("held policy lost")
	}
	want.Tasks[0].RunPolicy = RunPolicyEnabled
	snap, err = graph.ReplacePending(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Tasks[0].RunPolicy != RunPolicyEnabled {
		t.Fatal("release policy lost")
	}
	for _, invalid := range []PendingSubgraph{
		{Tasks: []PendingTask{{ID: "bad", Info: ""}}},
		{Tasks: []PendingTask{{ID: "../bad", Info: "bad"}}},
		{Tasks: []PendingTask{{ID: "bad", Info: "bad", RunPolicy: "auto"}}},
		{Tasks: []PendingTask{{ID: "same", Info: "one"}, {ID: "same", Info: "two"}}},
		{Tasks: want.Tasks, Edges: []Edge{{From: "missing", To: "work:1:planner"}}},
	} {
		before := graph.Snapshot()
		if _, err := graph.ReplacePending(context.Background(), invalid); !errors.Is(err, ErrInvalidPending) {
			t.Fatalf("invalid input: %v", err)
		}
		if !reflect.DeepEqual(before, graph.Snapshot()) {
			t.Fatal("invalid input changed graph")
		}
	}
}
