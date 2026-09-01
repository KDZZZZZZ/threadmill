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
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
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
	state, req, err := graph.ensureHelpRequest(root.Executor.ID, "call-1", "need evidence", nil)
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
	if _, _, err := first.ensureHelpRequest(firstRoot.Planner.ID, "earlier", "first", nil); err != nil {
		t.Fatal(err)
	}
	want, _, err := first.ensureHelpRequest(firstRoot.Executor.ID, "same-call", "second", nil)
	if err != nil {
		t.Fatal(err)
	}

	second := newGraph()
	secondRoot := second.AddTask()
	got, _, err := second.ensureHelpRequest(secondRoot.Executor.ID, "same-call", "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID {
		t.Fatalf("request IDs = %q and %q, want stable identity", want.ID, got.ID)
	}
}

func TestRequestHelpRejectsSingleCriticalPathUnit(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})
	graph.help.bind(helpTestRunner(graph, nil))

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), root.Executor.ID),
		agenttool.Call{
			ID:   "call-1",
			Name: coordRequestHelpName,
			Arguments: json.RawMessage(`{
				"reason":"split the work",
				"units":[{
					"id":"core",
					"goal":"implement core",
					"admission_reason":"critical_path",
					"inputs":[],
					"writes":["core.go"],
					"depends_on":[],
					"deliverable":"implementation and evidence"
				}]
			}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "critical_path requires at least two units") {
		t.Fatalf("request error = %v, want single critical_path rejection", err)
	}
	if got := len(graph.helps); got != 0 {
		t.Fatalf("persisted help requests = %d, want 0", got)
	}
}

func TestRequestHelpRequiresStructuredUnitsForNewRequest(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})
	graph.help.bind(helpTestRunner(graph, nil))

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), root.Executor.ID),
		agenttool.Call{
			ID:        "call-1",
			Name:      coordRequestHelpName,
			Arguments: json.RawMessage(`{"reason":"legacy free text"}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "units are required") {
		t.Fatalf("request error = %v, want structured units rejection", err)
	}
	if got := len(graph.helps); got != 0 {
		t.Fatalf("persisted help requests = %d, want 0", got)
	}
}

func TestRequestHelpRequiresCallID(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	tools := graph.HelpTools(nil)
	graph.help.bind(helpTestRunner(graph, nil))

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), root.Executor.ID),
		agenttool.Call{
			Name: coordRequestHelpName,
			Arguments: json.RawMessage(`{
				"reason":"offload evidence",
				"units":[{
					"id":"evidence",
					"goal":"gather evidence",
					"admission_reason":"context_offload",
					"inputs":[],
					"writes":[],
					"depends_on":[],
					"deliverable":"evidence"
				}]
			}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "call id is required") {
		t.Fatalf("request error = %v, want required call id", err)
	}
}

func TestRequestHelpRejectsIncompleteUnitShape(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})
	graph.help.bind(helpTestRunner(graph, nil))

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), root.Executor.ID),
		agenttool.Call{
			ID:   "call-1",
			Name: coordRequestHelpName,
			Arguments: json.RawMessage(`{
				"reason":"incomplete frontier",
				"units":[{
					"id":"evidence",
					"goal":"gather evidence",
					"admission_reason":"context_offload",
					"deliverable":"evidence"
				}]
			}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "inputs, writes, and depends_on arrays") {
		t.Fatalf("request error = %v, want incomplete unit rejection", err)
	}
}

func TestRequestHelpRejectsMissingDeclaredInput(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})
	run := helpTestRunner(graph, nil)
	run.stores.Files = vfs.NewStore(t.TempDir())
	graph.help.bind(run)

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), root.Executor.ID),
		agenttool.Call{
			ID:   "call-1",
			Name: coordRequestHelpName,
			Arguments: json.RawMessage(`{
				"reason":"delegate an isolated parser",
				"units":[{
					"id":"parser",
					"goal":"implement parser",
					"admission_reason":"context_offload",
					"inputs":["generated/schema.json"],
					"writes":["parser.go"],
					"depends_on":[],
					"deliverable":"implementation and evidence"
				}]
			}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), `unit "parser" input "generated/schema.json" is not available`) {
		t.Fatalf("request error = %v, want missing input rejection", err)
	}
	if got := len(graph.helps); got != 0 {
		t.Fatalf("persisted help requests = %d, want 0", got)
	}
}

func TestRequestHelpReplayDoesNotRevalidateInputs(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	units := []helpUnit{{
		ID:              "parser",
		Goal:            "implement parser",
		AdmissionReason: "context_offload",
		Inputs:          []string{"generated/schema.json"},
		Writes:          []string{"parser.go"},
		DependsOn:       []string{},
		Deliverable:     "implementation and evidence",
	}}
	state, _, err := graph.ensureHelpRequest(
		root.Executor.ID,
		"call-1",
		"delegate an isolated parser",
		units,
	)
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	run := helpTestRunner(graph, nil)
	run.stores.Files = vfs.NewStore(t.TempDir())
	graph.help.bind(run)

	result := make(chan error, 1)
	go func() {
		_, err := tools[coordRequestHelpName].Execute(
			agenttool.WithAgentID(context.Background(), root.Executor.ID),
			agenttool.Call{
				ID:   "call-1",
				Name: coordRequestHelpName,
				Arguments: json.RawMessage(`{
					"reason":"delegate an isolated parser",
					"units":[{
						"id":"parser",
						"goal":"implement parser",
						"admission_reason":"context_offload",
						"inputs":["generated/schema.json"],
						"writes":["parser.go"],
						"depends_on":[],
						"deliverable":"implementation and evidence"
					}]
				}`),
			},
		)
		result <- err
	}()

	select {
	case err := <-result:
		t.Fatalf("replayed request returned before manager response: %v", err)
	case <-notified:
		if err := graph.DeclineHelp(state.ID); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replayed request neither notified manager nor returned")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestRequestHelpAcceptsParallelRaceAndPipelineFrontiers(t *testing.T) {
	tests := []struct {
		name  string
		units []helpUnit
	}{
		{
			name: "parallel greenfield",
			units: []helpUnit{
				{ID: "api", Goal: "implement API", AdmissionReason: "critical_path", Inputs: []string{}, Writes: []string{"api.go"}, DependsOn: []string{}, Deliverable: "implementation"},
				{ID: "cli", Goal: "implement CLI", AdmissionReason: "critical_path", Inputs: []string{}, Writes: []string{"cli.go"}, DependsOn: []string{}, Deliverable: "implementation"},
			},
		},
		{
			name: "isolated race may share writes",
			units: []helpUnit{
				{ID: "candidate-a", Goal: "try candidate A", AdmissionReason: "race", Inputs: []string{}, Writes: []string{"solver.go"}, DependsOn: []string{}, Deliverable: "candidate and evidence"},
				{ID: "candidate-b", Goal: "try candidate B", AdmissionReason: "race", Inputs: []string{}, Writes: []string{"solver.go"}, DependsOn: []string{}, Deliverable: "candidate and evidence"},
			},
		},
		{
			name: "pipeline dependency is declarative",
			units: []helpUnit{
				{ID: "schema", Goal: "define schema", AdmissionReason: "context_offload", Inputs: []string{}, Writes: []string{"schema.json"}, DependsOn: []string{}, Deliverable: "schema"},
				{ID: "consumer", Goal: "consume schema", AdmissionReason: "context_offload", Inputs: []string{}, Writes: []string{"consumer.go"}, DependsOn: []string{"schema"}, Deliverable: "implementation"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := newGraph()
			root := graph.AddTask()
			notified := make(chan string, 1)
			tools := graph.HelpTools(func(message string) { notified <- message })
			run := helpTestRunner(graph, nil)
			run.stores.Files = vfs.NewStore(t.TempDir())
			graph.help.bind(run)

			arguments, err := json.Marshal(struct {
				Reason string     `json:"reason"`
				Units  []helpUnit `json:"units"`
			}{Reason: "delegate ready work", Units: test.units})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := tools[coordRequestHelpName].Execute(
					agenttool.WithAgentID(ctx, root.Executor.ID),
					agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: arguments},
				)
				result <- err
			}()

			message := <-notified
			if !strings.Contains(message, "Frontier:") ||
				!strings.Contains(message, `"admission_reason"`) ||
				strings.Contains(message, `"inputs":null`) ||
				strings.Contains(message, `"writes":null`) ||
				strings.Contains(message, `"depends_on":null`) {
				t.Fatalf("notification = %q, want structured frontier", message)
			}
			requestID, ok := ParseHelpRequestID(message)
			if !ok {
				t.Fatalf("notification = %q, want request id", message)
			}
			if err := graph.DeclineHelp(requestID); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHelpNotificationListsLegalSources(t *testing.T) {
	tests := []struct {
		name      string
		requester func(Task) string
		prepare   func(*runner, Task)
		want      string
		reject    []string
	}{
		{
			name:      "root planner has none",
			requester: func(task Task) string { return task.Planner.ID },
			want:      "合法来源: 无（结束本回合）",
		},
		{
			name:      "executor sees ready planner",
			requester: func(task Task) string { return task.Executor.ID },
			prepare: func(run *runner, task Task) {
				run.markNodeOutput(task.Planner.ID, "plan")
			},
			want:   "合法来源: task-1:planner (ready)",
			reject: []string{"task-1:executor (", "task-1:verifier ("},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := newGraph()
			root := graph.AddTask()
			notified := make(chan string, 1)
			tools := graph.HelpTools(func(message string) { notified <- message })
			run := helpTestRunner(graph, nil)
			if test.prepare != nil {
				test.prepare(run, root)
			}
			graph.help.bind(run)

			arguments := json.RawMessage(`{
				"reason":"offload investigation",
				"units":[{
					"id":"investigate",
					"goal":"gather evidence",
					"admission_reason":"context_offload",
					"inputs":[],
					"writes":[],
					"depends_on":[],
					"deliverable":"evidence"
				}]
			}`)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := tools[coordRequestHelpName].Execute(
					agenttool.WithAgentID(ctx, test.requester(root)),
					agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: arguments},
				)
				result <- err
			}()

			message := <-notified
			if !strings.Contains(message, test.want) {
				t.Fatalf("notification = %q, want %q", message, test.want)
			}
			for _, reject := range test.reject {
				if strings.Contains(message, reject) {
					t.Fatalf("notification = %q, rejects %q", message, reject)
				}
			}
			requestID, ok := ParseHelpRequestID(message)
			if !ok {
				t.Fatalf("notification = %q, want request id", message)
			}
			if err := graph.DeclineHelp(requestID); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProvideHelpJoinCycleReportsLegalSources(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	graph.HelpTools(nil)
	state, req, err := graph.ensureHelpRequest(
		root.Executor.ID,
		"call-1",
		"need evidence",
		[]helpUnit{{
			ID:              "evidence",
			Goal:            "gather evidence",
			AdmissionReason: "context_offload",
			Inputs:          []string{},
			Writes:          []string{},
			DependsOn:       []string{},
			Deliverable:     "evidence",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	graph.help.byID[state.ID] = req

	_, err = graph.help.provide(state.ID, []PendingSpawn{{
		From: root.Verifier.ID,
		Info: "gather evidence",
	}})
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("provide error = %v, want ErrJoinCycle", err)
	}
	if !strings.Contains(err.Error(), "合法来源: task-1:planner") {
		t.Fatalf("provide error = %v, want legal source", err)
	}
}

func TestProvideHelpRejectsSourceCreatedBySameBatch(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	graph.HelpTools(nil)
	state, req, err := graph.ensureHelpRequest(
		root.Executor.ID,
		"call-1",
		"need parallel implementation",
		[]helpUnit{
			{
				ID:              "first",
				Goal:            "implement first part",
				AdmissionReason: "critical_path",
				Inputs:          []string{},
				Writes:          []string{"first.go"},
				DependsOn:       []string{},
				Deliverable:     "first implementation",
			},
			{
				ID:              "second",
				Goal:            "implement second part",
				AdmissionReason: "critical_path",
				Inputs:          []string{},
				Writes:          []string{"second.go"},
				DependsOn:       []string{},
				Deliverable:     "second implementation",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	graph.help.byID[state.ID] = req

	_, err = graph.help.provide(state.ID, []PendingSpawn{
		{From: root.Planner.ID, Info: "implement first part"},
		{From: "task-2:planner", Info: "implement second part"},
	})
	if !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("provide error = %v, want ErrUnknownNode", err)
	}
	if got := len(graph.Snapshot().Tasks); got != 1 {
		t.Fatalf("tasks = %d, want unchanged graph", got)
	}
}

func TestProvideHelpReportsSourceReadiness(t *testing.T) {
	graph := newGraph()
	root := graph.AddTask()
	graph.HelpTools(nil)
	run := helpTestRunner(graph, nil)
	run.markNodeOutput(root.Planner.ID, "plan")
	graph.help.bind(run)
	state, req, err := graph.ensureHelpRequest(
		root.Executor.ID,
		"call-1",
		"need implementation",
		[]helpUnit{{
			ID:              "implementation",
			Goal:            "implement feature",
			AdmissionReason: "context_offload",
			Inputs:          []string{},
			Writes:          []string{"feature.go"},
			DependsOn:       []string{},
			Deliverable:     "implementation",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	graph.help.byID[state.ID] = req

	args, err := json.Marshal(map[string]any{
		"action":     "provide_help",
		"request_id": state.ID,
		"spawns": []map[string]string{{
			"from": root.Planner.ID,
			"info": "implement feature",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := executeGraphTool(t, GraphTools(graph), coordOrchestrateName, string(args))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Sources []struct {
			NodeID      string `json:"node_id"`
			OutputReady bool   `json:"output_ready"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 ||
		result.Sources[0].NodeID != root.Planner.ID ||
		!result.Sources[0].OutputReady {
		t.Fatalf("sources = %#v, want ready planner", result.Sources)
	}

	retryArgs, err := json.Marshal(map[string]any{
		"action":     "provide_help",
		"request_id": state.ID,
		"spawns": []map[string]string{{
			"from": root.Executor.ID,
			"info": "ignored retry payload",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err = executeGraphTool(t, GraphTools(graph), coordOrchestrateName, string(retryArgs))
	if err != nil {
		t.Fatal(err)
	}
	result.Sources = nil
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 1 || result.Sources[0].NodeID != root.Planner.ID {
		t.Fatalf("retry sources = %#v, want configured planner", result.Sources)
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
	if _, ok, err := run.restoredHelpResult(root, root.Executor.ID, "help-1", []helpChild{{task: child}}); err != nil || ok {
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
	if _, ok := tools["coordination_provideHelp"]; ok {
		t.Fatal("requester tools still expose manager help orchestration")
	}
	managerTools := GraphTools(first)
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
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence","units":[{"id":"evidence","goal":"gather evidence","admission_reason":"context_offload","inputs":[],"writes":[],"depends_on":[],"deliverable":"evidence"}]}`)},
		)
		result <- out
		errCh <- err
	}()
	<-notified
	provideArgs, err := json.Marshal(map[string]any{
		"action":     "provide_help",
		"request_id": helpRequestID(task.Executor.ID, "call-1"),
		"spawns": []map[string]string{{
			"from": "task-1:planner", "info": "gather evidence",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executeGraphTool(t, managerTools, coordOrchestrateName, string(provideArgs)); err != nil {
		t.Fatal(err)
	}
	firstOut := <-result
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	run.wg.Wait()
	if !strings.Contains(firstOut.Content, "[join pending]") || !strings.Contains(firstOut.Content, "task-2") {
		t.Fatalf("first request output = %q, want pending join notice", firstOut.Content)
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
	if !strings.Contains(out.Content, "[join pending]") || !strings.Contains(out.Content, "task-2") {
		t.Fatalf("restored request output = %q, want existing pending join", out.Content)
	}
	if got := second.Snapshot(); len(got.Tasks) != 2 {
		t.Fatalf("restored tasks = %d, want root plus one existing helper", len(got.Tasks))
	}
}

func TestHelpRequestRestoresStructuredFrontier(t *testing.T) {
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
	root := snap.Tasks[0]
	arguments := json.RawMessage(`{
		"reason":"offload evidence",
		"units":[{
			"id":"evidence",
			"goal":"gather evidence",
			"admission_reason":"context_offload",
			"inputs":[],
			"writes":[],
			"depends_on":[],
			"deliverable":"evidence report"
		}]
	}`)

	firstNotified := make(chan string, 1)
	firstTools := first.HelpTools(func(message string) { firstNotified <- message })
	first.help.bind(helpTestRunner(first, nil))
	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := firstTools[coordRequestHelpName].Execute(
			agenttool.WithAgentID(firstCtx, root.Executor.ID),
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: arguments},
		)
		firstResult <- err
	}()
	message := <-firstNotified
	if !strings.Contains(message, `"id":"evidence"`) {
		t.Fatalf("first notification = %q, want structured frontier", message)
	}
	firstCancel()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request error = %v, want context cancellation", err)
	}

	second, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	secondNotified := make(chan string, 1)
	secondTools := second.HelpTools(func(message string) { secondNotified <- message })
	second.help.bind(helpTestRunner(second, nil))
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	secondResult := make(chan error, 1)
	go func() {
		_, err := secondTools[coordRequestHelpName].Execute(
			agenttool.WithAgentID(secondCtx, root.Executor.ID),
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: arguments},
		)
		secondResult <- err
	}()
	message = <-secondNotified
	if !strings.Contains(message, `"id":"evidence"`) || !strings.Contains(message, `"goal":"gather evidence"`) {
		t.Fatalf("restored notification = %q, want structured frontier", message)
	}
	requestID, ok := ParseHelpRequestID(message)
	if !ok {
		t.Fatalf("restored notification = %q, want request id", message)
	}
	if err := second.DeclineHelp(requestID); err != nil {
		t.Fatal(err)
	}
	if err := <-secondResult; err != nil {
		t.Fatal(err)
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
			agenttool.Call{ID: "call-1", Name: coordRequestHelpName, Arguments: json.RawMessage(`{"reason":"need evidence","units":[{"id":"evidence","goal":"gather evidence","admission_reason":"context_offload","inputs":[],"writes":[],"depends_on":[],"deliverable":"evidence"}]}`)},
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
	state, _, err := graph.ensureHelpRequest(root.Executor.ID, "call-1", "need evidence", nil)
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

	got, err = run.runHelp(context.Background(), root, root.Executor.ID, state.ID, children)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[join pending]") || !strings.Contains(got, child.task.ID) {
		t.Fatalf("help result = %q, want pending join notice", got)
	}
}

func TestAddHelpPreservesPublishedTaskReference(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	published := graph.AddTask()
	if err := graph.recordOutcome(published.ID, nil); err != nil {
		t.Fatal(err)
	}
	graph.mu.Lock()
	graph.publishedTaskID = published.ID
	graph.mu.Unlock()
	current := graph.AddTask()
	state, _, err := graph.ensureHelpRequest(current.Executor.ID, "call-1", "need evidence", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := graph.addHelp(state.ID, current.Executor.ID, []PendingSpawn{{
		From: current.Planner.ID,
		Info: "gather evidence",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PublishedTaskID != published.ID {
		t.Fatalf("published task = %q, want %q", snapshot.PublishedTaskID, published.ID)
	}
}

func helpTestRunner(graph *Graph, progress ProgressStore) *runner {
	join := &joinCoordinator{graph: graph, sessions: make(map[string][]JoinProgress)}
	run := &runner{
		graph:      graph,
		stores:     Stores{Memory: ctxgraph.NewStore()},
		assemble:   func(Task) (Roles, error) { return instantRoles(), nil },
		progress:   progress,
		cancel:     func() {},
		childDone:  make(map[string]chan taskResult),
		nodeDone:   make(map[string]chan struct{}),
		nodeOutput: make(map[string]string),
		started:    make(map[string]struct{}),
		join:       join,
	}
	join.bind(run)
	return run
}
