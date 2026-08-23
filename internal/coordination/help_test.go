package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestProvideHelpRetriesSinkWithoutSpawningAgain(t *testing.T) {
	graph := newGraph()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := snap.Tasks[0]
	sinkErr := errors.New("sink failed")
	fail := true
	if err := graph.SetTaskSink(func(tasks []Task) error {
		for _, task := range tasks {
			if fail && task.Info == "gather evidence" {
				return sinkErr
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	graph.HelpTools(nil)
	state, req, err := graph.ensureHelpRequest(root.Executor.ID, "call-1", "need evidence")
	if err != nil {
		t.Fatal(err)
	}
	graph.help.byID[state.ID] = req
	spawns := []PendingSpawn{{From: root.Planner.ID, Info: "gather evidence"}}
	if _, err := graph.help.provide(state.ID, spawns); !errors.Is(err, sinkErr) {
		t.Fatalf("first provide error = %v, want sink error", err)
	}
	if got := len(graph.Snapshot().Tasks); got != 2 {
		t.Fatalf("tasks after failed sink = %d, want 2", got)
	}
	select {
	case <-req.configured:
		t.Fatal("request resumed before task info sink succeeded")
	default:
	}

	fail = false
	if _, err := graph.help.provide(state.ID, spawns); err != nil {
		t.Fatalf("retry provide error = %v", err)
	}
	if got := len(graph.Snapshot().Tasks); got != 2 {
		t.Fatalf("tasks after retry = %d, want no duplicate helper", got)
	}
	select {
	case <-req.configured:
	default:
		t.Fatal("request remained paused after task info sink succeeded")
	}
}

func TestHelpRequestIDDoesNotDependOnCreationOrder(t *testing.T) {
	t.Parallel()

	first := newGraph()
	firstRoot := first.AddTask()
	if _, _, err := first.ensureHelpRequest(firstRoot.Planner.ID, "earlier", "first"); err != nil {
		t.Fatal(err)
	}
	want, _, err := first.ensureHelpRequest(firstRoot.Executor.ID, "same-call", "second")
	if err != nil {
		t.Fatal(err)
	}

	second := newGraph()
	secondRoot := second.AddTask()
	got, _, err := second.ensureHelpRequest(secondRoot.Executor.ID, "same-call", "second")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID {
		t.Fatalf("request IDs = %q and %q, want stable identity", want.ID, got.ID)
	}
}

func TestRestoredHelpResultDoesNotReuseRegularJoinMarker(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	child, err := graph.Spawn(root.Planner.ID, root.Executor.ID)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Save(root.ID, TaskProgress{Merged: []string{root.Executor.ID}}); err != nil {
		t.Fatal(err)
	}
	run := helpTestRunner(graph, progress)
	if _, ok, err := run.restoredHelpResult(root.ID, "help-1", []helpChild{{task: child}}); err != nil || ok {
		if err != nil {
			t.Logf("restoredHelpResult() error = %v", err)
		}
		t.Fatal("regular join marker was mistaken for a restored help result")
	}
}

func TestHelpRequestRestoresConfiguredChildren(t *testing.T) {
	path := t.TempDir() + "/graph.json"
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	root, err := first.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := root.Tasks[0]
	notified := make(chan struct{}, 1)
	tools := first.HelpTools(func(string) { notified <- struct{}{} })
	run := helpTestRunner(first, progress)
	run.nodeOutput[task.Planner.ID] = "root plan"
	first.help.bind(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan agenttool.Output, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := tools[coordRequestHelpName].Execute(
			agenttool.WithAgentID(ctx, task.Executor.ID),
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence"}`)},
		)
		result <- out
		errCh <- err
	}()
	<-notified
	provideArgs, err := json.Marshal(map[string]any{
		"request_id": helpRequestID(task.Executor.ID, "call-1"),
		"spawns": []map[string]string{{
			"from": "task-1:planner", "info": "gather evidence",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[coordProvideHelpName].Execute(ctx, agenttool.Call{
		ID:        "provide-1",
		Name:      coordProvideHelpName,
		Arguments: provideArgs,
	}); err != nil {
		t.Fatal(err)
	}
	firstOut := <-result
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	run.wg.Wait()
	if !strings.Contains(firstOut.Content, "gather evidence") {
		t.Fatalf("first request output = %q, want helper report", firstOut.Content)
	}

	second, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	second.SetProgressStore(progress)
	secondTools := second.HelpTools(func(string) { t.Fatal("configured request notified manager again") })
	secondRun := helpTestRunner(second, progress)
	second.help.bind(secondRun)
	out, err := secondTools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(ctx, task.Executor.ID),
		agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence"}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "gather evidence") {
		t.Fatalf("restored request output = %q, want existing helper report", out.Content)
	}
	if got := second.Snapshot(); len(got.Tasks) != 2 {
		t.Fatalf("restored tasks = %d, want root plus one existing helper", len(got.Tasks))
	}
}

func TestDeclinedHelpRequestRestoresWithoutNotifyingManagerAgain(t *testing.T) {
	path := t.TempDir() + "/graph.json"
	first, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := first.ReplacePending(context.Background(), PendingSubgraph{
		Roots: []PendingRoot{{Info: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := snap.Tasks[0]
	notified := make(chan string, 1)
	tools := first.HelpTools(func(message string) { notified <- message })
	first.help.bind(helpTestRunner(first, nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan agenttool.Output, 1)
	errCh := make(chan error, 1)
	go func() {
		out, err := tools[coordRequestHelpName].Execute(
			agenttool.WithAgentID(ctx, task.Planner.ID),
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence"}`)},
		)
		result <- out
		errCh <- err
	}()
	message := <-notified
	requestID, ok := ParseHelpRequestID(message)
	if !ok {
		t.Fatalf("ParseHelpRequestID(%q) did not find request", message)
	}
	if err := first.DeclineHelp(requestID); err != nil {
		t.Fatal(err)
	}
	out := <-result
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "未提供帮助") {
		t.Fatalf("declined request output = %q", out.Content)
	}

	second, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	secondTools := second.HelpTools(func(string) { t.Fatal("declined request notified manager again") })
	second.help.bind(helpTestRunner(second, nil))
	out, err = secondTools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(ctx, task.Planner.ID),
		agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence"}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Content, "未提供帮助") {
		t.Fatalf("restored declined request output = %q", out.Content)
	}
	if got := len(second.Snapshot().Tasks); got != 1 {
		t.Fatalf("restored tasks = %d, want no helper task", got)
	}
}

func TestRegularJoinLeavesConfiguredHelpForRequestTool(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	state, _, err := graph.ensureHelpRequest(root.Executor.ID, "call-1", "need evidence")
	if err != nil {
		t.Fatal(err)
	}
	children, _, err := graph.addHelp(state.ID, root.Executor.ID, []PendingSpawn{{
		From: root.Planner.ID,
		Info: "gather evidence",
	}})
	if err != nil {
		t.Fatal(err)
	}
	child := children[0]
	run := helpTestRunner(graph, nil)
	run.started[child.task.ID] = struct{}{}
	run.nodeOutput[root.Planner.ID] = "root plan"
	run.childCh(child.task.ID) <- taskResult{output: "evidence"}

	got, _, err := run.joinIncoming(context.Background(), joinRequest{
		node: root.Executor, task: root, input: "root input",
		outputs: map[string]string{}, merged: map[string]bool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "root input" {
		t.Fatalf("regular join consumed help result: %q", got)
	}

	got, err = run.runHelp(context.Background(), root, state.ID, children)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "evidence") {
		t.Fatalf("help result = %q, want evidence", got)
	}
}

func helpTestRunner(graph *Graph, progress ProgressStore) *runner {
	return &runner{
		graph:      graph,
		stores:     Stores{Memory: ctxgraph.NewStore()},
		assemble:   func(Task) (Roles, error) { return instantRoles(), nil },
		progress:   progress,
		cancel:     func() {},
		childDone:  make(map[string]chan taskResult),
		nodeDone:   make(map[string]chan struct{}),
		nodeOutput: make(map[string]string),
		started:    make(map[string]struct{}),
	}
}
