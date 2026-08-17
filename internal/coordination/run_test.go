package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestGraphRunUnknownTask(t *testing.T) {
	t.Parallel()

	_, err := newGraph().Run(
		context.Background(),
		"task-missing",
		"in",
		ctxgraph.NewStore(),
		recordingAssemble(nil),
	)
	if !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("Run() error = %v, want %v", err, ErrUnknownTask)
	}
}

func TestGraphRunPlannerExecutorVerifierInOrder(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	task := graph.AddTask()
	var steps []string
	got, err := graph.Run(
		context.Background(),
		task.ID,
		"in",
		ctxgraph.NewStore(),
		recordingAssemble(&steps),
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantSteps := []string{
		task.ID + ":" + RolePlanner,
		task.ID + ":" + RoleExecutor,
		task.ID + ":" + RoleVerifier,
	}
	if strings.Join(steps, " ") != strings.Join(wantSteps, " ") {
		t.Fatalf("steps = %v, want %v", steps, wantSteps)
	}
	if got != "in/planner/executor/verifier" {
		t.Fatalf("Run() = %q, want in/planner/executor/verifier", got)
	}
}

func TestGraphRunResumesCanceledTaskWithoutReplayingFinishedRoles(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	task := graph.AddTask()
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)

	execStarted := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-execStarted
		cancel()
	}()
	_, err = graph.Run(ctx, task.ID, "in", ctxgraph.NewStore(), func(Task) (Roles, error) {
		return Roles{
			Planner: askerFunc(func(_ context.Context, query string) (string, error) {
				return query + "/planner", nil
			}),
			Executor: askerFunc(func(ctx context.Context, _ string) (string, error) {
				close(execStarted)
				<-ctx.Done()
				return "", ctx.Err()
			}),
			Verifier: instantAsker(),
		}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	var resumed []string
	got, err := graph.Run(context.Background(), task.ID, "in", ctxgraph.NewStore(), func(Task) (Roles, error) {
		return Roles{
			Planner: askerFunc(func(_ context.Context, query string) (string, error) {
				resumed = append(resumed, "planner:"+query)
				return "unused", nil
			}),
			Executor: askerFunc(func(_ context.Context, query string) (string, error) {
				resumed = append(resumed, "executor:"+query)
				return query + "/executor", nil
			}),
			Verifier: askerFunc(func(_ context.Context, query string) (string, error) {
				resumed = append(resumed, "verifier:"+query)
				return query + "/verifier", nil
			}),
		}, nil
	})
	if err != nil {
		t.Fatalf("resume Run() error = %v", err)
	}
	if strings.Join(resumed, " ") != "executor:in/planner verifier:in/planner/executor" {
		t.Fatalf("resumed asks = %v, want executor then verifier with saved planner output", resumed)
	}
	if got != "in/planner/executor/verifier" {
		t.Fatalf("resume Run() = %q, want in/planner/executor/verifier", got)
	}
	if _, ok, err := progress.Load(task.ID); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("progress kept after the task completed")
	}
}

func TestGraphRunResumesSpawnedChildWithoutReplayingFinishedRoles(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)

	execStarted := make(chan struct{})
	childFinished := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-execStarted
		cancel()
	}()
	_, err = graph.Run(ctx, root.ID, "in", ctxgraph.NewStore(), func(task Task) (Roles, error) {
		if task.ID == child.ID {
			return Roles{
				Planner:  instantAsker(),
				Executor: instantAsker(),
				Verifier: askerFunc(func(_ context.Context, query string) (string, error) {
					close(childFinished)
					return query, nil
				}),
			}, nil
		}
		return Roles{
			Planner: instantAsker(),
			Executor: askerFunc(func(ctx context.Context, _ string) (string, error) {
				<-childFinished
				close(execStarted)
				<-ctx.Done()
				return "", ctx.Err()
			}),
			Verifier: instantAsker(),
		}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	var childAsks []string
	got, err := graph.Run(context.Background(), root.ID, "in", ctxgraph.NewStore(), func(task Task) (Roles, error) {
		if task.ID != child.ID {
			return instantRoles(), nil
		}
		return Roles{
			Planner: askerFunc(func(_ context.Context, query string) (string, error) {
				childAsks = append(childAsks, "planner:"+query)
				return query, nil
			}),
			Executor: askerFunc(func(_ context.Context, query string) (string, error) {
				childAsks = append(childAsks, "executor:"+query)
				return query, nil
			}),
			Verifier: askerFunc(func(_ context.Context, query string) (string, error) {
				childAsks = append(childAsks, "verifier:"+query)
				return query, nil
			}),
		}, nil
	})
	if err != nil {
		t.Fatalf("resume Run() error = %v", err)
	}
	if len(childAsks) != 0 {
		t.Fatalf("child replayed finished roles: %v", childAsks)
	}
	if got != "in" {
		t.Fatalf("resume Run() = %q, want in", got)
	}
}

func TestGraphRunResumesInProgressReact(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	task := graph.AddTask()
	dir := t.TempDir()
	progress, err := NewDirProgressStore(dir + "/task")
	if err != nil {
		t.Fatal(err)
	}
	react, err := agent.NewDirCheckpointStore(dir + "/react")
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)

	started := make(chan struct{})
	var mu sync.Mutex
	resuming := false
	var resumeFirst []agent.Message
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if !strings.Contains(request.SystemPrompt, "规划 Agent") {
			return agent.AssistantMessage{Content: roleReply(request.SystemPrompt)}, nil
		}
		mu.Lock()
		if resuming && resumeFirst == nil {
			resumeFirst = append([]agent.Message(nil), request.Messages...)
		}
		mu.Unlock()
		if hasToolResult(request.Messages) {
			return agent.AssistantMessage{Content: "planned"}, nil
		}
		return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
			ID:        "call-1",
			Name:      "echo",
			Arguments: json.RawMessage(`{}`),
		}}}, nil
	})
	assemble := Assemble(
		ctxgraph.NewStore(),
		provider,
		agent.FileAgents{},
		[]agenttool.Tool{&blockingTool{started: started}},
		0,
		react,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	if _, err := graph.Run(ctx, task.ID, "in", ctxgraph.NewStore(), assemble); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	mu.Lock()
	resuming = true
	mu.Unlock()
	got, err := graph.Run(context.Background(), task.ID, "in", ctxgraph.NewStore(), assemble)
	if err != nil {
		t.Fatalf("resume Run() error = %v", err)
	}
	if !hasToolResult(resumeFirst) {
		t.Fatalf("resume planner messages = %#v, want the paused react including a tool result", resumeFirst)
	}
	if got != "verified" {
		t.Fatalf("resume Run() = %q, want verified", got)
	}
}

func TestGraphRunStartsSpawnWhenRoleStarts(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

	execStarted := make(chan struct{})
	execRelease := make(chan struct{})
	childStarted := make(chan struct{})

	assemble := func(task Task) (Roles, error) {
		if task.ID == root.ID {
			return Roles{
				Planner:  instantAsker(),
				Executor: gatedAsker(execStarted, execRelease),
				Verifier: instantAsker(),
			}, nil
		}
		if task.ID == child.ID {
			return Roles{
				Planner:  gatedAsker(childStarted, nil),
				Executor: instantAsker(),
				Verifier: instantAsker(),
			}, nil
		}
		return instantRoles(), nil
	}

	done := runAsync(t, graph, assemble, root.ID)
	waitChan(t, execStarted)
	waitChan(t, childStarted)
	close(execRelease)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunSameTaskStageWaitsForPreviousComplete(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	mustSpawn(t, graph, root.Planner.ID, root.Executor.ID)

	plannerStarted := make(chan struct{})
	plannerRelease := make(chan struct{})
	execStarted := make(chan struct{})

	assemble := func(task Task) (Roles, error) {
		if task.ID != root.ID {
			return instantRoles(), nil
		}
		return Roles{
			Planner:  gatedAsker(plannerStarted, plannerRelease),
			Executor: gatedAsker(execStarted, nil),
			Verifier: instantAsker(),
		}, nil
	}

	done := runAsync(t, graph, assemble, root.ID)
	waitChan(t, plannerStarted)
	assertNotClosed(t, execStarted)
	close(plannerRelease)
	waitChan(t, execStarted)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunCompleteWaitsForIncomingJoin(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	_ = mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

	childVRelease := make(chan struct{})
	rootVStarted := make(chan struct{})

	assemble := func(task Task) (Roles, error) {
		if task.ID == root.ID {
			return Roles{
				Planner:  instantAsker(),
				Executor: instantAsker(),
				Verifier: gatedAsker(rootVStarted, nil),
			}, nil
		}
		return Roles{
			Planner:  instantAsker(),
			Executor: instantAsker(),
			Verifier: gatedAsker(nil, childVRelease),
		}, nil
	}

	done := runAsync(t, graph, assemble, root.ID)
	waitChan(t, rootVStarted)
	assertNotDone(t, done)
	close(childVRelease)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunNestedSpawnStartsWithParentRole(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	_ = mustSpawn(t, graph, child.Executor.ID, root.Verifier.ID)

	childEStarted := make(chan struct{})
	childERelease := make(chan struct{})
	grandStarted := make(chan struct{})

	assemble := func(task Task) (Roles, error) {
		switch task.ID {
		case child.ID:
			return Roles{
				Planner:  instantAsker(),
				Executor: gatedAsker(childEStarted, childERelease),
				Verifier: instantAsker(),
			}, nil
		default:
			if task.SpawnedFrom == child.ID {
				return Roles{
					Planner:  gatedAsker(grandStarted, nil),
					Executor: instantAsker(),
					Verifier: instantAsker(),
				}, nil
			}
			return instantRoles(), nil
		}
	}

	done := runAsync(t, graph, assemble, root.ID)
	waitChan(t, childEStarted)
	waitChan(t, grandStarted)
	close(childERelease)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunIsolatesToolsByTaskEnv(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	store := ctxgraph.NewStore()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	store.Save(root.Env.ID, ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "shared",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	})

	assemble := func(task Task) (Roles, error) {
		tools := agenttool.Bind(store, task.Env.ID, agenttool.MemoryTools(nil, nil))
		add := mustTool(t, tools, "memory_add_to_subgraph")
		return Roles{
			Planner: askerFunc(func(ctx context.Context, query string) (string, error) {
				args, err := json.Marshal(map[string]any{
					"subgraph_id": "from-" + task.ID,
					"node_ids":    []string{"n1"},
				})
				if err != nil {
					return "", err
				}
				_, err = add.Execute(ctx, agenttool.Call{
					ID:        "call-1",
					Name:      "memory_add_to_subgraph",
					Arguments: args,
				})
				return query, err
			}),
			Executor: askerFunc(func(_ context.Context, query string) (string, error) {
				return query, nil
			}),
			Verifier: askerFunc(func(_ context.Context, query string) (string, error) {
				return query, nil
			}),
		}, nil
	}

	if _, err := graph.Run(context.Background(), root.ID, "in", store, assemble); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if nodes := store.Load(root.Env.ID).NodesInSubgraphs([]string{"from-" + child.ID}); len(nodes) != 0 {
		t.Fatalf("parent env saw child write: %#v", nodes)
	}
	if nodes := store.Load(child.Env.ID).NodesInSubgraphs([]string{"from-" + child.ID}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("child env missing its own write")
	}
	if nodes := store.Load(root.Env.ID).NodesInSubgraphs([]string{"from-" + root.ID}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("parent env missing its own write")
	}
}

func TestGraphRunAssembledReActIsolatesMemoryByEnv(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	store := ctxgraph.NewStore()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	store.Save(root.Env.ID, seededMemoryGraph())

	got, err := graph.Run(
		context.Background(),
		root.ID,
		"in",
		store,
		Assemble(
			store,
			reactMemoryProvider(),
			envMemoryAgents(),
			nil,
			0,
			nil,
		),
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got != "verified" {
		t.Fatalf("Run() = %q, want verified", got)
	}

	if nodes := ctxgraph.Clone("check").Graph.NodesInSubgraphs([]string{"mark-in", "mark-planned"}); len(nodes) != 0 {
		t.Fatalf("react write leaked to global graph: %#v", nodes)
	}
	if nodes := store.Load(root.Env.ID).NodesInSubgraphs([]string{"mark-in"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("parent env missing planner write")
	}
	if nodes := store.Load(root.Env.ID).NodesInSubgraphs([]string{"mark-planned"}); len(nodes) != 0 {
		t.Fatalf("parent env saw child planner write: %#v", nodes)
	}
	if nodes := store.Load(child.Env.ID).NodesInSubgraphs([]string{"mark-planned"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("child env missing planner write")
	}
	if nodes := store.Load(child.Env.ID).NodesInSubgraphs([]string{"mark-in"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("child env did not fork parent planner write")
	}
}

func TestGraphRunAssembledReActSharesMemoryWithinTask(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	store := ctxgraph.NewStore()
	task := graph.AddTask()
	store.Save(task.Env.ID, seededMemoryGraph())

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		switch {
		case strings.Contains(request.SystemPrompt, "规划 Agent"):
			if hasToolResult(request.Messages) {
				return agent.AssistantMessage{Content: "planned"}, nil
			}
			args, err := json.Marshal(map[string]any{
				"subgraph_id": "mark-in",
				"node_ids":    []string{"n1"},
			})
			if err != nil {
				return agent.AssistantMessage{}, err
			}
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "mem-1",
					Name:      "memory_add_to_subgraph",
					Arguments: args,
				}},
			}, nil
		case strings.Contains(request.SystemPrompt, "执行 Agent"):
			if hasToolResult(request.Messages) {
				content := lastToolContent(request.Messages)
				if !strings.Contains(content, `"n1"`) {
					return agent.AssistantMessage{}, fmt.Errorf("executor did not see planner memory: %s", content)
				}
				return agent.AssistantMessage{Content: "executed"}, nil
			}
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "mem-2",
					Name:      "memory_nodes_in",
					Arguments: json.RawMessage(`{"subgraph_ids":["mark-in"]}`),
				}},
			}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})

	got, err := graph.Run(
		context.Background(),
		task.ID,
		"in",
		store,
		Assemble(
			store,
			provider,
			envMemoryAgents(),
			nil,
			0,
			nil,
		),
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got != "verified" {
		t.Fatalf("Run() = %q, want verified", got)
	}
}

func TestAssembleBindsLeakingMemoryToolsToTaskEnv(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	store := ctxgraph.NewStore()
	store.Save("env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "local",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	})

	var calls int
	provider := stubProvider(func(_ context.Context, _ agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      "memory_add_to_subgraph",
					Arguments: json.RawMessage(`{"subgraph_id":"bound","node_ids":["n1"]}`),
				}},
			}, nil
		}
		return agent.AssistantMessage{Content: "done"}, nil
	})

	extra := agenttool.MemoryTools(func() ctxgraph.Copy {
		return ctxgraph.Clone("leak")
	}, ctxgraph.Update)
	roles, err := Assemble(
		store,
		provider,
		agent.FileAgents{},
		extra,
		0,
		nil,
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}

	got, err := roles.Planner.Ask(context.Background(), "write")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if got != "done" {
		t.Fatalf("Ask() = %q, want done", got)
	}
	if nodes := ctxgraph.Clone("check").Graph.NodesInSubgraphs([]string{"bound"}); len(nodes) != 0 {
		t.Fatalf("assemble write leaked to global graph: %#v", nodes)
	}
	if nodes := store.Load("env-1").NodesInSubgraphs([]string{"bound"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("assemble write did not stay in task env")
	}
}

type blockingTool struct {
	started chan struct{}
}

func (t *blockingTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        "echo",
		Description: "Echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (t *blockingTool) Execute(ctx context.Context, _ agenttool.Call) (agenttool.Output, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	<-ctx.Done()
	return agenttool.Output{}, ctx.Err()
}

type askerFunc func(context.Context, string) (string, error)

func (f askerFunc) Ask(ctx context.Context, query string) (string, error) {
	return f(ctx, query)
}

func instantAsker() Asker {
	return gatedAsker(nil, nil)
}

func instantRoles() Roles {
	return Roles{
		Planner:  instantAsker(),
		Executor: instantAsker(),
		Verifier: instantAsker(),
	}
}

func gatedAsker(started chan struct{}, release <-chan struct{}) Asker {
	return askerFunc(func(ctx context.Context, query string) (string, error) {
		if started != nil {
			select {
			case <-started:
			default:
				close(started)
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return query, nil
	})
}

func runAsync(t *testing.T, graph *Graph, assemble AssembleFunc, taskID string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := graph.Run(
			context.Background(),
			taskID,
			"in",
			ctxgraph.NewStore(),
			assemble,
		)
		done <- err
	}()
	return done
}

func waitChan(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func waitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Run")
		return nil
	}
}

func assertNotClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("signaled too early")
	default:
	}
}

func assertNotDone(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("Run finished too early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

type stubProvider func(context.Context, agent.Request) (agent.AssistantMessage, error)

func (f stubProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	return f(ctx, request)
}

func reactMemoryProvider() stubProvider {
	return func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if hasToolResult(request.Messages) {
			return agent.AssistantMessage{Content: roleReply(request.SystemPrompt)}, nil
		}
		if !strings.Contains(request.SystemPrompt, "规划 Agent") {
			return agent.AssistantMessage{Content: roleReply(request.SystemPrompt)}, nil
		}
		if !hasTool(request.Tools, "memory_add_to_subgraph") {
			return agent.AssistantMessage{}, fmt.Errorf("planner missing memory_add_to_subgraph")
		}
		args, err := json.Marshal(map[string]any{
			"subgraph_id": "mark-" + firstUserContent(request.Messages),
			"node_ids":    []string{"n1"},
		})
		if err != nil {
			return agent.AssistantMessage{}, err
		}
		return agent.AssistantMessage{
			ToolCalls: []agenttool.Call{{
				ID:        "mem-1",
				Name:      "memory_add_to_subgraph",
				Arguments: args,
			}},
		}, nil
	}
}

func roleReply(systemPrompt string) string {
	switch {
	case strings.Contains(systemPrompt, "规划 Agent"):
		return "planned"
	case strings.Contains(systemPrompt, "执行 Agent"):
		return "executed"
	default:
		return "verified"
	}
}

func hasTool(tools []agenttool.Definition, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func hasToolResult(messages []agent.Message) bool {
	for _, message := range messages {
		if message.Role == agent.RoleTool {
			return true
		}
	}
	return false
}

func firstUserContent(messages []agent.Message) string {
	for _, message := range messages {
		if message.Role == agent.RoleUser {
			return message.Content
		}
	}
	return ""
}

func lastToolContent(messages []agent.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleTool {
			return messages[i].Content
		}
	}
	return ""
}

func envMemoryAgents() agent.FileAgents {
	return agent.FileAgents{
		Planner: agent.FileAgent{
			Tools: []string{"memory_add_to_subgraph"},
		},
		Executor: agent.FileAgent{
			Tools: []string{"memory_nodes_in"},
		},
	}
}

func seededMemoryGraph() ctxgraph.Graph {
	return ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "shared",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	}
}

func recordingAssemble(steps *[]string) AssembleFunc {
	var mu sync.Mutex
	return func(task Task) (Roles, error) {
		roleAsker := func(role string) Asker {
			return askerFunc(func(_ context.Context, query string) (string, error) {
				if steps != nil {
					mu.Lock()
					*steps = append(*steps, task.ID+":"+role)
					mu.Unlock()
				}
				return query + "/" + role, nil
			})
		}
		return Roles{
			Planner:  roleAsker(RolePlanner),
			Executor: roleAsker(RoleExecutor),
			Verifier: roleAsker(RoleVerifier),
		}, nil
	}
}

func mustTool(t *testing.T, tools []agenttool.Tool, name string) agenttool.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}
