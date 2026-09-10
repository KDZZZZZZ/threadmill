package coordination

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOrdinaryTasksHaveIndependentActivationsAndUntypedEdges(t *testing.T) {
	t.Parallel()

	graph := New()
	first := graph.AddTask()
	second := graph.AddTask()
	if first.Env.ID == second.Env.ID {
		t.Fatal("independent tasks share an environment")
	}
	for _, task := range []Task{first, second} {
		for _, node := range task.Sequence() {
			want := fmt.Sprintf("%s:1:%s", task.ID, node.Role)
			if node.ID != want {
				t.Fatalf("node ID = %q, want activation-qualified %q", node.ID, want)
			}
		}
		if got := graph.Incoming(task.Planner.ID); len(got) != 0 {
			t.Fatalf("independent planner has implicit predecessors: %v", got)
		}
	}
	if _, found := reflect.TypeOf(Edge{}).FieldByName("Kind"); found {
		t.Fatal("ordinary edges still have a kind")
	}
	if _, found := reflect.TypeOf(Env{}).FieldByName("ParentID"); found {
		t.Fatal("task environment still encodes ancestry")
	}
}

func TestNodeOutputIsPairedImmutableAndDurable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	task := graph.AddTask()
	output := Output{Node: task.Planner, FilesRef: "files-1", MemoryRef: "memory-1", Report: "plan"}
	if err := graph.commitOutput(output); err != nil {
		t.Fatal(err)
	}
	before := graph.Snapshot()
	if err := graph.commitOutput(output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("replaying identical output changed the graph")
	}
	changed := output
	changed.FilesRef = "other-files"
	if err := graph.commitOutput(changed); err == nil {
		t.Fatal("replaced an immutable output")
	}
	if err := graph.commitOutput(Output{Node: task.Executor, FilesRef: "files-only"}); err == nil {
		t.Fatal("accepted output without paired memory")
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Output(task.Planner.ID); !ok || got != output {
		t.Fatalf("reopened output = %#v, %v", got, ok)
	}
	if !reflect.DeepEqual(before, reopened.Snapshot()) {
		t.Fatal("failed output commit changed durable state")
	}
}

func TestConnectUsesOrdinaryDAGDependencies(t *testing.T) {
	t.Parallel()
	graph := New()
	first, second := graph.AddTask(), graph.AddTask()
	if err := graph.Connect(first.Verifier.ID, second.Executor.ID); err != nil {
		t.Fatal(err)
	}
	got := graph.Incoming(second.Executor.ID)
	want := []Node{second.Planner, first.Verifier}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incoming = %#v, want %#v", got, want)
	}
	before := graph.Snapshot()
	if err := graph.Connect(first.Verifier.ID, second.Executor.ID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Snapshot(), before) {
		t.Fatal("duplicate edge changed graph")
	}
	for _, edge := range []Edge{{From: second.Verifier.ID, To: first.Planner.ID}, {From: first.Planner.ID, To: first.Planner.ID}, {From: "unknown", To: first.Planner.ID}, {From: first.Planner.ID, To: "unknown"}} {
		if err := graph.Connect(edge.From, edge.To); err == nil {
			t.Fatalf("accepted invalid edge %#v", edge)
		}
		if !reflect.DeepEqual(graph.Snapshot(), before) {
			t.Fatalf("invalid edge %#v mutated graph", edge)
		}
	}
}

func TestContinuePersistentTaskKeepsImmutableActivationHistory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "watch", Info: "first observation", Persistent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	first := snap.Tasks[0]
	output := Output{Node: first.Verifier, FilesRef: "first-files", MemoryRef: "first-memory", Report: "first observation"}
	if err := graph.commitOutput(output); err != nil {
		t.Fatal(err)
	}
	graph.tasks[0].Outcome = OutcomeIdle
	var projected Task
	if err := graph.SetTaskSink(func(tasks []Task) error { projected = tasks[0]; return nil }); err != nil {
		t.Fatal(err)
	}
	second, err := graph.Continue(first.ID, "second observation")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Activation != 2 || second.Env.ID == first.Env.ID || second.Outcome != OutcomeActive || second.Info != "second observation" {
		t.Fatalf("continued task = %#v", second)
	}
	if projected != second {
		t.Fatalf("task sink = %#v, want current activation", projected)
	}
	if got := graph.Incoming(second.Planner.ID); !reflect.DeepEqual(got, []Node{first.Verifier}) {
		t.Fatalf("continued input = %#v", got)
	}
	if got, ok := graph.Output(first.Verifier.ID); !ok || got != output {
		t.Fatalf("old output lost: %#v", got)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Snapshot(), reopened.Snapshot()) {
		t.Fatal("activation history changed across restart")
	}
	if len(reopened.Snapshot().Nodes) != 6 {
		t.Fatal("historical role nodes lost")
	}
}

func TestClosePersistentTaskCancelsOnlyItsOwnActivation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "first", Info: "observe first", Persistent: true}, {ID: "second", Info: "observe second", Persistent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	canceled := make(map[string]bool)
	for _, task := range snap.Tasks {
		graph.runners[task.ID] = &runner{task: task, cancel: func() { canceled[task.ID] = true }, nodeStarted: map[string]struct{}{}}
	}
	if err := graph.CloseTask("first"); err != nil {
		t.Fatal(err)
	}
	if !canceled["first"] || canceled["second"] {
		t.Fatalf("canceled tasks = %#v", canceled)
	}
	if err := graph.CloseTask("first"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := reopened.Task("first")
	second, _ := reopened.Task("second")
	if first.Outcome != OutcomeClosed || second.Outcome != OutcomeActive {
		t.Fatalf("reopened outcomes = %s, %s", first.Outcome, second.Outcome)
	}
	if _, err := graph.Continue("first", "resume closed task"); err == nil {
		t.Fatal("continued closed task")
	}
}

func TestCheckpointNodesParticipateInOrdinaryDependencies(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	task := graph.AddTask()
	pause := Node{ID: task.Planner.ID + ":pause", TaskID: task.ID, Role: RolePlanner}
	resume := Node{ID: task.Planner.ID + ":resume", TaskID: task.ID, Role: RolePlanner}
	for _, node := range []Node{pause, resume} {
		if err := graph.addCheckpointNode(node); err != nil {
			t.Fatal(err)
		}
	}
	if err := graph.commitOutput(Output{Node: pause, FilesRef: "pause-files", MemoryRef: "pause-memory"}); err != nil {
		t.Fatal(err)
	}
	if err := graph.Connect(pause.ID, resume.ID); err != nil {
		t.Fatal(err)
	}
	if err := graph.Connect(resume.ID, task.Planner.ID); err != nil {
		t.Fatal(err)
	}
	if got := graph.Incoming(resume.ID); !reflect.DeepEqual(got, []Node{pause}) {
		t.Fatalf("resume input = %#v", got)
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Snapshot(), reopened.Snapshot()) {
		t.Fatal("checkpoint nodes lost on restart")
	}
}

func TestNewEdgesRequireLiveOrCommittedSourceNodes(t *testing.T) {
	t.Parallel()
	graph := New()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{Tasks: []PendingTask{{ID: "watch", Info: "observe", Persistent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	first := snap.Tasks[0]
	orphan := Node{ID: first.Planner.ID + ":unused", TaskID: first.ID, Role: RolePlanner}
	if err := graph.addCheckpointNode(orphan); err != nil {
		t.Fatal(err)
	}
	if err := graph.commitOutput(Output{Node: first.Verifier, FilesRef: "old-files", MemoryRef: "old-memory"}); err != nil {
		t.Fatal(err)
	}
	graph.tasks[0].Outcome = OutcomeIdle
	current, err := graph.Continue(first.ID, "continue observing")
	if err != nil {
		t.Fatal(err)
	}
	consumer := graph.AddTask()
	before := graph.Snapshot()
	if err := graph.Connect(orphan.ID, consumer.Planner.ID); err == nil {
		t.Fatal("uncommitted historical source can never become ready")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("invalid source changed graph")
	}
	if err := graph.Connect(first.Verifier.ID, consumer.Planner.ID); err != nil {
		t.Fatalf("committed historical source: %v", err)
	}
	if err := graph.Connect(current.Planner.ID, consumer.Planner.ID); err != nil {
		t.Fatalf("current source may still become ready: %v", err)
	}
}
