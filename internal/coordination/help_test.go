package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestRequestHelpRejectsSingleCriticalPathUnit(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), task.Executor.ID),
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
}

func TestRequestHelpRequiresStructuredUnitsForNewRequest(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), task.Executor.ID),
		agenttool.Call{
			ID:        "call-1",
			Name:      coordRequestHelpName,
			Arguments: json.RawMessage(`{"reason":"legacy free text"}`),
		},
	)
	if err == nil || !strings.Contains(err.Error(), "units are required") {
		t.Fatalf("request error = %v, want structured units rejection", err)
	}
}

func TestRequestHelpRequiresCallID(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	tools := graph.HelpTools(nil)

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), task.Executor.ID),
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
	graph := New()
	task := graph.AddTask()
	tools := graph.HelpTools(func(string) {
		t.Fatal("invalid request notified manager")
	})

	_, err := tools[coordRequestHelpName].Execute(
		agenttool.WithAgentID(context.Background(), task.Executor.ID),
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

func TestRequestHelpPreservesParallelRaceAndPipelineFrontiers(t *testing.T) {
	for _, test := range []struct {
		name  string
		units []helpUnit
	}{
		{
			name: "parallel work",
			units: []helpUnit{
				{ID: "api", Goal: "implement API", AdmissionReason: "critical_path", Inputs: []string{}, Writes: []string{"api.go"}, DependsOn: []string{}, Deliverable: "implementation"},
				{ID: "cli", Goal: "implement CLI", AdmissionReason: "critical_path", Inputs: []string{}, Writes: []string{"cli.go"}, DependsOn: []string{}, Deliverable: "implementation"},
			},
		},
		{
			name: "isolated race with shared writes",
			units: []helpUnit{
				{ID: "candidate-a", Goal: "try A", AdmissionReason: "race", Inputs: []string{}, Writes: []string{"solver.go"}, DependsOn: []string{}, Deliverable: "candidate"},
				{ID: "candidate-b", Goal: "try B", AdmissionReason: "race", Inputs: []string{}, Writes: []string{"solver.go"}, DependsOn: []string{}, Deliverable: "candidate"},
			},
		},
		{
			name: "declarative pipeline",
			units: []helpUnit{
				{ID: "schema", Goal: "define schema", AdmissionReason: "context_offload", Inputs: []string{}, Writes: []string{"schema.json"}, DependsOn: []string{}, Deliverable: "schema"},
				{ID: "consumer", Goal: "consume schema", AdmissionReason: "context_offload", Inputs: []string{}, Writes: []string{"consumer.go"}, DependsOn: []string{"schema"}, Deliverable: "implementation"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			graph := New()
			task := graph.AddTask()
			ctx := helpContext(t, graph)
			notified := make(chan string, 1)
			tools := graph.HelpTools(func(message string) { notified <- message })
			call := helpCall()
			args, err := json.Marshal(struct {
				Reason string     `json:"reason"`
				Units  []helpUnit `json:"units"`
			}{Reason: "delegate ready work", Units: test.units})
			if err != nil {
				t.Fatal(err)
			}
			call.Arguments = args
			done := runHelpTask(ctx, graph, task, Stores{Memory: ctxgraph.NewStore()}, func(current Task) (Roles, error) {
				return requestHelpRoles(tools, current, call, nil), nil
			})
			message := awaitHelpMessage(t, ctx, notified)
			if !strings.Contains(message, "Frontier:") ||
				strings.Contains(message, `"inputs":null`) ||
				strings.Contains(message, `"writes":null`) ||
				strings.Contains(message, `"depends_on":null`) {
				t.Fatalf("notification lost structured frontier: %q", message)
			}
			for _, unit := range test.units {
				if !strings.Contains(message, `"id":"`+unit.ID+`"`) {
					t.Fatalf("notification lost unit %q: %q", unit.ID, message)
				}
			}
			requestID, _ := ParseHelpRequestID(message)
			if err := graph.DeclineHelp(requestID); err != nil {
				t.Fatal(err)
			}
			awaitHelpDone(t, ctx, done)
		})
	}
}

func TestHelpWithoutReturnEdgeDoesNotWaitForHelper(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	stores := Stores{Memory: ctxgraph.NewStore()}
	result := make(chan string, 1)
	assemble := func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, helpCall(), result), nil
	}
	done := runHelpTask(ctx, graph, task, stores, assemble)
	message := awaitHelpMessage(t, ctx, notified)
	pauseID, resumeID := helpNotificationEndpoints(t, message)
	pause, ok := graph.Output(pauseID)
	if !ok || pause.Node.TaskID != task.ID || pause.MemoryRef == "" {
		t.Fatalf("pause output = %+v, %v, want committed requester state", pause, ok)
	}
	if _, err := provideHelpTasks(t, graph, message, PendingSubgraph{
		Tasks: []PendingTask{{ID: "background", Info: "keep collecting evidence", Persistent: true}},
	}); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	if incoming := graph.Incoming(resumeID); len(incoming) != 1 || incoming[0].ID != pauseID {
		t.Fatalf("resume inputs = %+v, want only frozen pause", incoming)
	}
	background, ok := graph.Task("background")
	if !ok || !background.Persistent || background.Outcome != OutcomeActive {
		t.Fatalf("independent helper = %+v, %v", background, ok)
	}
	if notice := <-result; !strings.Contains(notice, "[input ready]") || !strings.Contains(notice, "input:") {
		t.Fatalf("help result = %q, want ready input notice", notice)
	}
}

func TestHelpWaitsOnlyForExplicitResumeInputs(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	stores := Stores{Memory: ctxgraph.NewStore()}
	returningStarted := make(chan struct{})
	releaseReturning := make(chan struct{})
	detachedStarted := make(chan struct{})
	result := make(chan string, 1)
	assemble := func(current Task) (Roles, error) {
		switch current.ID {
		case task.ID:
			return requestHelpRoles(tools, current, helpCall(), result), nil
		case "returning":
			roles := instantRoles()
			roles.Planner = gatedAsker(returningStarted, releaseReturning)
			roles.Verifier = askerFunc(func(context.Context, string) (string, error) {
				return "private helper report", nil
			})
			return roles, nil
		default:
			roles := instantRoles()
			roles.Planner = gatedAsker(detachedStarted, make(chan struct{}))
			return roles, nil
		}
	}
	done := runHelpTask(ctx, graph, task, stores, assemble)
	message := awaitHelpMessage(t, ctx, notified)
	pauseID, resumeID := helpNotificationEndpoints(t, message)
	if _, err := provideHelpTasks(t, graph, message, PendingSubgraph{
		Tasks: []PendingTask{
			{ID: "returning", Info: "produce required evidence"},
			{ID: "detached", Info: "keep collecting", Persistent: true},
		},
		Edges: []Edge{
			{From: pauseID, To: "returning:1:planner"},
			{From: "returning:1:verifier", To: resumeID},
			{From: pauseID, To: "detached:1:planner"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	detached, _ := graph.Task("detached")
	detachedDone := runHelpTask(ctx, graph, detached, stores, assemble)
	awaitHelpSignal(t, ctx, returningStarted)
	awaitHelpSignal(t, ctx, detachedStarted)
	select {
	case err := <-done:
		t.Fatalf("requester finished before required output: %v", err)
	default:
	}
	close(releaseReturning)
	awaitHelpDone(t, ctx, done)
	select {
	case err := <-detachedDone:
		t.Fatalf("detached task stopped with requester: %v", err)
	default:
	}
	if notice := <-result; !strings.Contains(notice, "[input ready]") || strings.Contains(notice, "private helper report") {
		t.Fatalf("help result = %q, want ready notice without candidate dialogue", notice)
	}
}

func TestHelpAllowsDependenciesBetweenNewTasks(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	assemble := func(current Task) (Roles, error) {
		if current.ID == task.ID {
			return requestHelpRoles(tools, current, helpCall(), nil), nil
		}
		return instantRoles(), nil
	}
	done := runHelpTask(ctx, graph, task, Stores{Memory: ctxgraph.NewStore()}, assemble)
	message := awaitHelpMessage(t, ctx, notified)
	pauseID, resumeID := helpNotificationEndpoints(t, message)
	if _, err := provideHelpTasks(t, graph, message, PendingSubgraph{
		Tasks: []PendingTask{{ID: "schema", Info: "define schema"}, {ID: "consumer", Info: "consume schema"}},
		Edges: []Edge{
			{From: pauseID, To: "schema:1:planner"},
			{From: "schema:1:verifier", To: "consumer:1:planner"},
			{From: "consumer:1:verifier", To: resumeID},
		},
	}); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	for _, id := range []string{"schema", "consumer"} {
		if task, _ := graph.Task(id); task.Outcome != OutcomeDone {
			t.Fatalf("task %s outcome = %s", id, task.Outcome)
		}
	}
}

func TestProvideHelpRejectsCycleWithoutAddingTasks(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	done := runHelpTask(ctx, graph, task, Stores{Memory: ctxgraph.NewStore()}, func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, helpCall(), nil), nil
	})
	message := awaitHelpMessage(t, ctx, notified)
	_, resumeID := helpNotificationEndpoints(t, message)
	_, err := provideHelpTasks(t, graph, message, PendingSubgraph{
		Tasks: []PendingTask{{ID: "cyclic", Info: "depends on the blocked role"}},
		Edges: []Edge{{From: task.Verifier.ID, To: "cyclic:1:planner"}, {From: "cyclic:1:verifier", To: resumeID}},
	})
	if err == nil {
		t.Fatal("accepted cycle through suspended requester")
	}
	if snapshot := graph.Snapshot(); len(snapshot.Tasks) != 1 {
		t.Fatalf("tasks after rejected graph = %d", len(snapshot.Tasks))
	}
	requestID, _ := ParseHelpRequestID(message)
	if err := graph.DeclineHelp(requestID); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
}

func TestProvideHelpRetriesTaskSinkWithoutAddingDuplicates(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	sinkErr := errors.New("sink failed")
	var fail atomic.Bool
	fail.Store(true)
	if err := graph.SetTaskSink(func(tasks []Task) error {
		for _, task := range tasks {
			if fail.Load() && task.Info == "gather evidence" {
				return sinkErr
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	done := runHelpTask(ctx, graph, task, Stores{Memory: ctxgraph.NewStore()}, func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, helpCall(), nil), nil
	})
	message := awaitHelpMessage(t, ctx, notified)
	pending := PendingSubgraph{Tasks: []PendingTask{{ID: "evidence", Info: "gather evidence"}}}
	if _, err := provideHelpTasks(t, graph, message, pending); !errors.Is(err, sinkErr) {
		t.Fatalf("first provide error = %v, want sink failure", err)
	}
	select {
	case err := <-done:
		t.Fatalf("requester resumed before task sink succeeded: %v", err)
	default:
	}
	fail.Store(false)
	if _, err := provideHelpTasks(t, graph, message, pending); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	if snapshot := graph.Snapshot(); len(snapshot.Tasks) != 2 {
		t.Fatalf("tasks after retry = %d, want requester and one helper", len(snapshot.Tasks))
	}
}

func TestRequestHelpRejectsMissingDeclaredInput(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	var notifications atomic.Int64
	tools := graph.HelpTools(func(string) { notifications.Add(1) })
	call := helpCall()
	call.Arguments = json.RawMessage(strings.ReplaceAll(string(call.Arguments), `"inputs":[]`, `"inputs":["generated/schema.json"]`))
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}
	t.Cleanup(func() { _ = stores.Files.Close() })
	done := runHelpTask(ctx, graph, task, stores, func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, call, nil), nil
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `input "generated/schema.json" is not available`) {
			t.Fatalf("request error = %v, want missing declared input", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if notifications.Load() != 0 {
		t.Fatal("invalid request notified manager")
	}
}

func TestConfiguredHelpSurvivesGraphReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	done := runHelpTask(ctx, graph, task, Stores{Memory: ctxgraph.NewStore()}, func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, helpCall(), nil), nil
	})
	message := awaitHelpMessage(t, ctx, notified)
	pending := PendingSubgraph{Tasks: []PendingTask{{ID: "evidence", Info: "gather evidence"}}}
	if _, err := provideHelpTasks(t, graph, message, pending); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	restored, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	restored.HelpTools(nil)
	if _, err := provideHelpTasks(t, restored, message, pending); err != nil {
		t.Fatalf("retry configured help after reload: %v", err)
	}
	if snapshot := restored.Snapshot(); len(snapshot.Tasks) != 2 {
		t.Fatalf("restored tasks = %d, want no duplicate helper", len(snapshot.Tasks))
	}
	pauseID, resumeID := helpNotificationEndpoints(t, message)
	if incoming := restored.Incoming(resumeID); len(incoming) != 1 || incoming[0].ID != pauseID {
		t.Fatalf("restored resume input = %+v", incoming)
	}
}

func TestConcurrentHelpRequestsFreezeTheirOwnTask(t *testing.T) {
	graph := New()
	first, second := graph.AddTask(), graph.AddTask()
	ctx := helpContext(t, graph)
	notified := make(chan string, 2)
	tools := graph.HelpTools(func(message string) { notified <- message })
	stores := Stores{Memory: ctxgraph.NewStore()}
	assemble := func(current Task) (Roles, error) {
		return requestHelpRoles(tools, current, helpCall(), nil), nil
	}
	firstDone := runHelpTask(ctx, graph, first, stores, assemble)
	secondDone := runHelpTask(ctx, graph, second, stores, assemble)
	seen := make(map[string]bool)
	for range 2 {
		message := awaitHelpMessage(t, ctx, notified)
		pauseID, resumeID := helpNotificationEndpoints(t, message)
		pause, ok := graph.Output(pauseID)
		if !ok || seen[pause.Node.TaskID] || !strings.HasPrefix(resumeID, pause.Node.TaskID+":") {
			t.Fatalf("request routed to wrong task: %+v, %v", pause, ok)
		}
		seen[pause.Node.TaskID] = true
		requestID, _ := ParseHelpRequestID(message)
		if err := graph.DeclineHelp(requestID); err != nil {
			t.Fatal(err)
		}
	}
	awaitHelpDone(t, ctx, firstDone)
	awaitHelpDone(t, ctx, secondDone)
}

func helpCall() agenttool.Call {
	return agenttool.Call{
		ID: "help-call", Name: coordRequestHelpName,
		Arguments: json.RawMessage(`{"reason":"offload evidence","units":[{"id":"evidence","goal":"gather evidence","admission_reason":"context_offload","inputs":[],"writes":[],"depends_on":[],"deliverable":"evidence report"}]}`),
	}
}

func requestHelpRoles(tools map[string]agenttool.Tool, task Task, call agenttool.Call, result chan<- string) Roles {
	roles := instantRoles()
	roles.Executor = askerFunc(func(ctx context.Context, _ string) (string, error) {
		out, err := tools[coordRequestHelpName].Execute(agenttool.WithAgentID(ctx, task.Executor.ID), call)
		if err == nil && result != nil {
			result <- out.Content
		}
		return out.Content, err
	})
	return roles
}

func helpContext(t *testing.T, graph *Graph) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	graph.SetRunContext(ctx)
	t.Cleanup(func() { cancel(); graph.WaitRuns() })
	return ctx
}

func runHelpTask(ctx context.Context, graph *Graph, task Task, stores Stores, assemble AssembleFunc) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := graph.Run(ctx, task.ID, "perform task", stores, assemble)
		done <- err
	}()
	return done
}

func awaitHelpMessage(t *testing.T, ctx context.Context, messages <-chan string) string {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-ctx.Done():
		t.Fatal("help request did not notify manager:", ctx.Err())
		return ""
	}
}

func awaitHelpSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func awaitHelpDone(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("task did not finish:", ctx.Err())
	}
}

func helpNotificationEndpoints(t *testing.T, message string) (string, string) {
	t.Helper()
	var pause, resume string
	for _, line := range strings.Split(message, "\n") {
		if value, ok := strings.CutPrefix(line, "Pause: "); ok {
			pause = value
		}
		if value, ok := strings.CutPrefix(line, "Resume: "); ok {
			resume = value
		}
	}
	if pause == "" || resume == "" || pause == resume {
		t.Fatalf("notification has no distinct pause/resume: %q", message)
	}
	return pause, resume
}

func provideHelpTasks(t *testing.T, graph *Graph, message string, pending PendingSubgraph) (agenttool.Output, error) {
	t.Helper()
	requestID, ok := ParseHelpRequestID(message)
	if !ok {
		t.Fatalf("notification has no request ID: %q", message)
	}
	arguments, err := json.Marshal(orchestrateArgs{
		Action: "provide_help", RequestID: requestID, Tasks: pending.Tasks, Edges: pending.Edges,
	})
	if err != nil {
		t.Fatal(err)
	}
	return executeGraphTool(t, GraphTools(graph), coordOrchestrateName, string(arguments))
}
