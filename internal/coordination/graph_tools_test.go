package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestGraphToolsReplacePendingChangesDeclaredEdges(t *testing.T) {
	t.Parallel()
	graph := New()
	tools := GraphTools(graph)
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{
		"action":"replace_pending","tasks":[{"id":"source","info":"produce"},{"id":"consumer","info":"consume"}]
	}`); err != nil {
		t.Fatal(err)
	}
	source, _ := graph.Task("source")
	consumer, _ := graph.Task("consumer")
	for _, from := range []string{source.Planner.ID, source.Verifier.ID} {
		arguments, err := json.Marshal(orchestrateArgs{
			Action: "replace_pending",
			Tasks:  []PendingTask{{ID: source.ID, Info: "produce"}, {ID: consumer.ID, Info: "consume"}},
			Edges:  []Edge{{From: from, To: consumer.Planner.ID}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := executeGraphTool(t, tools, coordOrchestrateName, string(arguments)); err != nil {
			t.Fatal(err)
		}
		incoming := graph.Incoming(consumer.Planner.ID)
		if len(incoming) != 1 || incoming[0].ID != from {
			t.Fatalf("consumer incoming = %+v, want %s", incoming, from)
		}
	}
	if snapshot := graph.Snapshot(); len(snapshot.Tasks) != 2 {
		t.Fatalf("tasks = %d, want existing ordinary tasks", len(snapshot.Tasks))
	}
}

func TestGraphToolsExposeSingleManagerOrchestrationTool(t *testing.T) {
	t.Parallel()
	graph := New()
	tools := GraphTools(graph)
	if len(tools) != 1 || tools[0].Definition().Name != coordOrchestrateName {
		t.Fatalf("manager graph tools = %v, want only %s", toolNames(tools), coordOrchestrateName)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{"action":"replace_pending","tasks":[{"info":"work"}]}`); err != nil {
		t.Fatal(err)
	}
	if snapshot := graph.Snapshot(); len(snapshot.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snapshot.Tasks))
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tools[0].Definition().InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["roots"] != nil || schema.Properties["spawns"] != nil {
		t.Fatal("schema still exposes tree classifications")
	}
}

func TestOrchestrationReceiptsKeepReferencesWithoutRepeatingReports(t *testing.T) {
	graph := New()
	ctx := helpContext(t, graph)
	stores := Stores{Memory: ctxgraph.NewStore()}
	source := graph.AddTask()
	report := strings.Repeat("preserved evidence\n", 4096)
	_, err := graph.Run(ctx, source.ID, "produce evidence", stores, func(Task) (Roles, error) {
		roles := instantRoles()
		roles.Verifier = askerFunc(func(context.Context, string) (string, error) { return report, nil })
		return roles, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, result agenttool.Output) {
		t.Helper()
		var receipt Snapshot
		if err := json.Unmarshal([]byte(result.Content), &receipt); err != nil {
			t.Fatal(err)
		}
		if len(result.Content) >= len(report) {
			t.Errorf("%s receipt repeats report bytes: receipt=%d, report=%d", name, len(result.Content), len(report))
		}
		stored, ok := graph.Output(source.Verifier.ID)
		if !ok || stored.Report != report {
			t.Fatal("canonical evidence changed when rendering receipt")
		}
		projection, err := graph.Snapshot().PromptProjection()
		if err != nil {
			t.Fatal(err)
		}
		var current Snapshot
		if err := json.Unmarshal(projection, &current); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, output := range current.Outputs {
			if output.Node.ID == source.Verifier.ID && output.Report == report {
				found = true
			}
		}
		if !found || len(receipt.Outputs) != len(current.Outputs) {
			t.Fatal("current graph must retain full evidence and receipt must retain all references")
		}
		for i, output := range receipt.Outputs {
			want := current.Outputs[i]
			if output.Node != want.Node || output.FilesRef != want.FilesRef || output.MemoryRef != want.MemoryRef {
				t.Fatalf("%s receipt changed output identity: %+v", name, output.Node)
			}
		}
		t.Logf("%s: receipt=%d bytes, preserved report=%d bytes", name, len(result.Content), len(report))
	}
	args, _ := json.Marshal(orchestrateArgs{Action: "replace_pending", Tasks: []PendingTask{{ID: source.ID}}})
	result, err := executeGraphTool(t, GraphTools(graph), coordOrchestrateName, string(args))
	if err != nil {
		t.Fatal(err)
	}
	check("replace_pending", result)

	requester := graph.AddTask()
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	done := runHelpTask(ctx, graph, requester, stores, func(task Task) (Roles, error) {
		return requestHelpRoles(tools, task, helpCall(), nil), nil
	})
	message := awaitHelpMessage(t, ctx, notified)
	result, err = provideHelpTasks(t, graph, message, PendingSubgraph{})
	if err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	// The requester may finish after the receipt; compare at the source boundary.
	if strings.Contains(result.Content, "preserved evidence") {
		t.Error("provide_help receipt repeats the source report")
	}
	stored, ok := graph.Output(source.Verifier.ID)
	if !ok || stored.Report != report {
		t.Fatal("provide_help modified canonical evidence")
	}
}

func TestOrchestrateRejectsFieldsFromAnotherAction(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, args, want string }{
		{"request ID in replacement", `{"action":"replace_pending","request_id":"help/id","tasks":[{"info":"work"}]}`, "request_id is not valid"},
		{"task ID in help", `{"action":"provide_help","request_id":"help/id","task_id":"work"}`, "task_id and input are not valid"},
		{"graph fields in continuation", `{"action":"continue_task","task_id":"work","tasks":[]}`, "tasks and edges are not valid"},
		{"input in close", `{"action":"close_task","task_id":"work","input":"next"}`, "input is not valid"},
		{"legacy roots", `{"action":"replace_pending","roots":[{"info":"work"}]}`, `unknown field "roots"`},
		{"legacy spawns", `{"action":"provide_help","request_id":"help/id","spawns":[]}`, `unknown field "spawns"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := executeGraphTool(t, GraphTools(New()), coordOrchestrateName, test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGraphToolsReplacePendingRejectsCycle(t *testing.T) {
	t.Parallel()
	graph := New()
	task := graph.AddTask()
	arguments, err := json.Marshal(orchestrateArgs{
		Action: "replace_pending", Tasks: []PendingTask{{ID: task.ID, Info: "cycle"}},
		Edges: []Edge{{From: task.Verifier.ID, To: task.Planner.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := graph.Snapshot()
	if _, err := executeGraphTool(t, GraphTools(graph), coordOrchestrateName, string(arguments)); err == nil {
		t.Fatal("accepted a directed cycle")
	}
	if after := graph.Snapshot(); after.Revision != before.Revision || len(after.Tasks) != len(before.Tasks) {
		t.Fatalf("rejected graph changed state: %+v", after)
	}
}

func TestGraphToolNilGraph(t *testing.T) {
	t.Parallel()
	_, err := executeGraphTool(t, GraphTools(nil), coordOrchestrateName, `{"action":"replace_pending"}`)
	if err == nil || !strings.Contains(err.Error(), "nil graph") {
		t.Fatalf("error = %v, want nil graph", err)
	}
}

func runFileTask(t *testing.T, graph *Graph, task Task, stores Stores, path, content string, failure error) {
	t.Helper()
	_, err := graph.Run(context.Background(), task.ID, "produce checkpoint", stores, func(Task) (Roles, error) {
		roles := instantRoles()
		roles.Executor = askerFunc(func(context.Context, string) (string, error) {
			return "checkpoint written", stores.Files.View(task.Env.ID).Write(path, []byte(content))
		})
		if failure != nil {
			roles.Verifier = askerFunc(func(context.Context, string) (string, error) { return "", failure })
		}
		return roles, nil
	})
	if failure == nil && err != nil || failure != nil && !errors.Is(err, failure) {
		t.Fatalf("task execution = %v, want %v", err, failure)
	}
}

func toolNames(tools []agenttool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Definition().Name)
	}
	return names
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
		return tool.Execute(context.Background(), agenttool.Call{ID: "c-" + name, Name: name, Arguments: json.RawMessage(arguments)})
	}
	t.Fatalf("tool %q not found", name)
	return agenttool.Output{}, nil
}

func TestGraphToolsCreateOrdinaryTaskEdges(t *testing.T) {
	t.Parallel()

	graph := New()
	_, err := executeGraphTool(t, GraphTools(graph), coordOrchestrateName, `{
		"action":"replace_pending",
		"tasks":[{"id":"source","info":"produce evidence"},{"id":"consumer","info":"use evidence"}],
		"edges":[{"from":"source:1:verifier","to":"consumer:1:planner"}]
	}`)
	if err != nil {
		t.Fatalf("create ordinary tasks and edge: %v", err)
	}
	if got := graph.Snapshot(); len(got.Tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(got.Tasks))
	}
	incoming := graph.Incoming("consumer:1:planner")
	if len(incoming) != 1 || incoming[0].ID != "source:1:verifier" {
		t.Fatalf("consumer incoming = %+v, want declared source", incoming)
	}
}

func TestGraphToolsContinueAndClosePersistentTask(t *testing.T) {
	t.Parallel()
	graph := New()
	base := t.TempDir()
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(base)}
	t.Cleanup(func() { _ = stores.Files.Close() })
	tools := GraphTools(graph)
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{
		"action":"replace_pending","tasks":[{"id":"ongoing","info":"first request","persistent":true}]
	}`); err != nil {
		t.Fatal(err)
	}
	first, _ := graph.Task("ongoing")
	runFileTask(t, graph, first, stores, "progress.txt", "first", nil)
	if task, _ := graph.Task(first.ID); task.Outcome != OutcomeIdle {
		t.Fatalf("persistent task outcome = %q, want idle", task.Outcome)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{
		"action":"continue_task","task_id":"ongoing","input":"follow-up request"
	}`); err != nil {
		t.Fatal(err)
	}
	continued, _ := graph.Task(first.ID)
	if continued.Activation != first.Activation+1 || continued.Env.ID == first.Env.ID || continued.Planner.ID == first.Planner.ID {
		t.Fatalf("continuation reused activation state: %+v", continued)
	}
	var plannerInput string
	_, err := graph.Run(context.Background(), continued.ID, "", stores, func(Task) (Roles, error) {
		roles := instantRoles()
		roles.Planner = askerFunc(func(_ context.Context, input string) (string, error) {
			plannerInput = input
			return "planned follow-up", nil
		})
		roles.Executor = askerFunc(func(context.Context, string) (string, error) {
			return "continued", stores.Files.View(continued.Env.ID).Write("progress.txt", []byte("second"))
		})
		return roles, nil
	})
	if err != nil || !strings.Contains(plannerInput, "follow-up request") {
		t.Fatalf("continuation input = %q, error %v", plannerInput, err)
	}
	output, ok := graph.Output(first.Verifier.ID)
	if !ok {
		t.Fatal("continuation lost the previous committed output")
	}
	if got, err := stores.Files.View(output.FilesRef).Read("progress.txt"); err != nil || string(got) != "first" {
		t.Fatalf("previous activation file = %q, %v", got, err)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{"action":"close_task","task_id":"ongoing"}`); err != nil {
		t.Fatal(err)
	}
	if task, _ := graph.Task(first.ID); task.Outcome != OutcomeClosed {
		t.Fatalf("closed task outcome = %q", task.Outcome)
	}
	latest, _ := graph.Output(continued.Verifier.ID)
	if got, err := stores.Files.View(latest.FilesRef).Read("progress.txt"); err != nil || string(got) != "second" {
		t.Fatalf("published continuation = %q, %v", got, err)
	}
	if _, err := executeGraphTool(t, tools, coordOrchestrateName, `{"action":"continue_task","task_id":"ongoing","input":"late"}`); err == nil {
		t.Fatal("closed task accepted a continuation")
	}
}
