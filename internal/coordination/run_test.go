package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestGraphRunRetainsIsolatedTaskOutputs(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()
	base := t.TempDir()
	files := vfs.NewStore(base)
	var live string
	assemble := func(task Task) (Roles, error) {
		return Roles{
			Planner: instantAsker(),
			Executor: askerFunc(func(_ context.Context, query string) (string, error) {
				dir, err := files.Materialize(task.Env.ID)
				if err != nil {
					return "", err
				}
				live = dir
				if err := os.WriteFile(filepath.Join(dir, "from-bash.txt"), []byte("from-live"), 0o640); err != nil {
					return "", err
				}
				return query + "/executor", nil
			}),
			Verifier: instantAsker(),
		}, nil
	}

	if _, err := graph.Run(context.Background(), task.ID, "in", Stores{Memory: ctxgraph.NewStore(), Files: files}, assemble); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "from-bash.txt")); !os.IsNotExist(err) {
		t.Fatalf("Run published before manager selection: %v", err)
	}
	got, err := files.View(task.Env.ID).Read("from-bash.txt")
	if err != nil || string(got) != "from-live" {
		t.Fatalf("retained from-bash.txt = %q, %v, want from-live", got, err)
	}
	if live == "" {
		t.Fatal("executor did not materialize")
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live dir still exists after Run: %v", err)
	}
	if stats := files.Stats(); stats.Environments == 0 || stats.LiveDirs != 0 {
		t.Fatalf("completed root snapshot stats = %+v, want retained immutable snapshots without live directories", stats)
	}
	if err := files.Discard(task.Env.ID); err != nil {
		t.Fatal(err)
	}
	output, ok := graph.Output(task.Verifier.ID)
	if !ok {
		t.Fatal("missing retained output")
	}
	got, err = files.View(output.FilesRef).Read("from-bash.txt")
	if err != nil || string(got) != "from-live" {
		t.Fatalf("archived file = %q, %v", got, err)
	}
}

func TestGraphRunDoesNotPublishFailedTask(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()
	base := t.TempDir()
	files := vfs.NewStore(base)
	wantErr := errors.New("executor failed")
	assemble := func(task Task) (Roles, error) {
		return Roles{
			Planner: instantAsker(),
			Executor: askerFunc(func(_ context.Context, _ string) (string, error) {
				if err := files.View(task.Env.ID).Write("failed.txt", []byte("must not publish")); err != nil {
					return "", err
				}
				return "", wantErr
			}),
			Verifier: instantAsker(),
		}, nil
	}

	if _, err := graph.Run(
		context.Background(),
		task.ID,
		"in",
		Stores{Memory: ctxgraph.NewStore(), Files: files},
		assemble,
	); !errors.Is(err, wantErr) {
		t.Fatalf("Run() error = %v, want %v", err, wantErr)
	}
	if _, err := os.Stat(filepath.Join(base, "failed.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed root published files: %v", err)
	}
}

func TestGraphRunUnknownTask(t *testing.T) {
	t.Parallel()

	_, err := New().Run(
		context.Background(),
		"task-missing",
		"in",
		Stores{Memory: ctxgraph.NewStore()},
		recordingAssemble(nil),
	)
	if !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("Run() error = %v, want %v", err, ErrUnknownTask)
	}
}

func TestGraphRunPlannerExecutorVerifierInOrder(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()
	var steps []string
	got, err := graph.Run(
		context.Background(),
		task.ID,
		"in",
		Stores{Memory: ctxgraph.NewStore()},
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

func TestGraphRunResumesRecoverableRoleErrors(t *testing.T) {
	t.Parallel()

	for name, recoverable := range map[string]error{
		"step slice":    agent.ErrMaxSteps,
		"memory format": fmt.Errorf("compact failed: %w", agent.ErrMemoryFormat),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			graph := New()
			task := graph.AddTask()
			calls := 0
			got, err := graph.Run(
				context.Background(),
				task.ID,
				"in",
				Stores{Memory: ctxgraph.NewStore()},
				func(Task) (Roles, error) {
					return Roles{
						Planner: askerFunc(func(_ context.Context, query string) (string, error) {
							calls++
							if calls == 1 {
								return "", recoverable
							}
							return query + "/planner", nil
						}),
						Executor: instantAsker(),
						Verifier: instantAsker(),
					}, nil
				},
			)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if calls != 2 {
				t.Fatalf("planner calls = %d, want 2", calls)
			}
			if got != "in/planner" {
				t.Fatalf("Run() = %q, want continued role output", got)
			}
		})
	}
}

func TestGraphRunReportsRepeatedRecoverableRoleErrorAsStall(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()
	calls := 0
	var reported error
	_, err := graph.RunWithReport(
		context.Background(),
		task.ID,
		"in",
		Stores{Memory: ctxgraph.NewStore()},
		func(Task) (Roles, error) {
			return Roles{
				Planner: askerFunc(func(context.Context, string) (string, error) {
					calls++
					return "", agent.ErrMemoryFormat
				}),
				Executor: instantAsker(),
				Verifier: instantAsker(),
			}, nil
		},
		func(_ Task, _ string, taskErr error) error {
			reported = taskErr
			return nil
		},
	)
	if !errors.Is(err, ErrRoleStalled) || !errors.Is(reported, ErrRoleStalled) {
		t.Fatalf("errors = (%v, %v), want %v", err, reported, ErrRoleStalled)
	}
	if calls < 2 || calls > 10 {
		t.Fatalf("planner calls = %d, want bounded recovery attempts", calls)
	}
}

func TestGraphRunVerifierReadsExecutorLiveWrite(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()
	files := vfs.NewStore(t.TempDir())
	assemble := func(task Task) (Roles, error) {
		return Roles{
			Planner: instantAsker(),
			Executor: askerFunc(func(_ context.Context, query string) (string, error) {
				dir, err := files.Materialize(task.Env.ID)
				if err != nil {
					return "", err
				}
				if err := os.WriteFile(filepath.Join(dir, "from-exec.txt"), []byte("from-live"), 0o640); err != nil {
					return "", err
				}
				return query + "/executor", nil
			}),
			Verifier: askerFunc(func(_ context.Context, query string) (string, error) {
				got, err := files.View(task.Env.ID).Read("from-exec.txt")
				if err != nil {
					return "", fmt.Errorf("verifier missed executor live write: %w", err)
				}
				if string(got) != "from-live" {
					return "", fmt.Errorf("from-exec.txt = %q, want from-live", got)
				}
				return query + "/verifier", nil
			}),
		}, nil
	}
	if _, err := graph.Run(context.Background(), task.ID, "in", Stores{Memory: ctxgraph.NewStore(), Files: files}, assemble); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunResumesCanceledTaskWithoutReplayingFinishedRoles(t *testing.T) {
	t.Parallel()

	graph := New()
	stores := Stores{Memory: ctxgraph.NewStore()}
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
	_, err = graph.Run(ctx, task.ID, "in", stores, func(Task) (Roles, error) {
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
	got, err := graph.Run(context.Background(), task.ID, "in", stores, func(Task) (Roles, error) {
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
	if _, ok, err := progress.Load(task.Env.ID); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("activation checkpoint missing after completion")
	}
}

func TestGraphRunResumesInProgressReact(t *testing.T) {
	graph := New()
	stores := Stores{Memory: ctxgraph.NewStore()}
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
		stores,
		provider,
		rolePromptAgents(),
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
	if _, err := graph.Run(ctx, task.ID, "in", stores, assemble); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	mu.Lock()
	resuming = true
	mu.Unlock()
	got, err := graph.Run(context.Background(), task.ID, "in", stores, assemble)
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

func TestGraphRunCompletesOnlyAfterReportSucceeds(t *testing.T) {
	t.Parallel()

	graphPath := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{Info: "report"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := snap.Tasks[0]
	progress, err := NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	reportErr := errors.New("report failed")
	var reported Task
	_, err = graph.RunWithReport(
		context.Background(), task.ID, "in", Stores{Memory: ctxgraph.NewStore()}, recordingAssemble(nil),
		func(task Task, _ string, _ error) error {
			reported = task
			if current, _ := graph.Task(task.ID); current.Outcome != OutcomeActive {
				t.Fatalf("graph outcome during report = %q, want active", current.Outcome)
			}
			return reportErr
		},
	)
	if !errors.Is(err, reportErr) {
		t.Fatalf("RunWithReport() error = %v, want %v", err, reportErr)
	}
	if reported.Outcome != OutcomeDone {
		t.Fatalf("reported outcome = %q, want done", reported.Outcome)
	}
	if current, _ := graph.Task(task.ID); current.Outcome != OutcomeActive {
		t.Fatalf("outcome after failed report = %q, want active", current.Outcome)
	}
	reopened, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	if current, _ := reopened.Task(task.ID); current.Outcome != OutcomeActive {
		t.Fatalf("persisted outcome after failed report = %q, want active", current.Outcome)
	}
	if _, ok, err := progress.Load(task.Env.ID); err != nil || !ok {
		t.Fatalf("progress after failed report = (%v, %v), want retained", ok, err)
	}

	_, err = graph.RunWithReport(
		context.Background(), task.ID, "in", Stores{Memory: ctxgraph.NewStore()}, recordingAssemble(nil),
		func(Task, string, error) error { return nil },
	)
	if err != nil {
		t.Fatalf("retry RunWithReport() error = %v", err)
	}
	if current, _ := graph.Task(task.ID); current.Outcome != OutcomeDone {
		t.Fatalf("outcome after report = %q, want done", current.Outcome)
	}
	if _, ok, err := progress.Load(task.Env.ID); err != nil || !ok {
		t.Fatalf("activation checkpoint after completion = (%v, %v), want retained", ok, err)
	}
}

func TestGraphRunRecordsCanceledOutcome(t *testing.T) {
	t.Parallel()

	graph := New()
	root := graph.AddTask()
	child := graph.AddTask()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	_, err := graph.Run(ctx, root.ID, "in", Stores{Memory: ctxgraph.NewStore()}, func(task Task) (Roles, error) {
		if task.ID == root.ID {
			return Roles{
				Planner: askerFunc(func(ctx context.Context, _ string) (string, error) {
					close(started)
					<-ctx.Done()
					return "", ctx.Err()
				}),
				Executor: instantAsker(),
				Verifier: instantAsker(),
			}, nil
		}
		return instantRoles(), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	got, ok := graph.Task(root.ID)
	if !ok || got.Outcome != OutcomeCanceled {
		t.Fatalf("root outcome = %+v, want canceled", got)
	}
	still, ok := graph.Task(child.ID)
	if !ok || still.Outcome != OutcomeActive {
		t.Fatalf("unrelated task outcome = %+v, want active", still)
	}
}

func TestGraphRunRecordsFailedOutcome(t *testing.T) {
	t.Parallel()

	graph := New()
	root := graph.AddTask()
	child := graph.AddTask()
	boom := errors.New("planner boom")
	_, err := graph.Run(context.Background(), root.ID, "in", Stores{Memory: ctxgraph.NewStore()}, func(Task) (Roles, error) {
		return Roles{
			Planner: askerFunc(func(context.Context, string) (string, error) {
				return "", boom
			}),
			Executor: instantAsker(),
			Verifier: instantAsker(),
		}, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want %v", err, boom)
	}
	got, ok := graph.Task(root.ID)
	if !ok || got.Outcome != OutcomeFailed {
		t.Fatalf("root outcome = %+v, want failed", got)
	}
	got, ok = graph.Task(child.ID)
	if !ok || got.Outcome != OutcomeActive {
		t.Fatalf("unrelated task outcome = %+v, want active", got)
	}
}

func TestReplacePendingRejectsStartedNodeChangeWhileExecuting(t *testing.T) {
	t.Parallel()

	graph := New()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{Info: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := snap.Tasks[0]
	plannerStarted := make(chan struct{})
	plannerRelease := make(chan struct{})
	done := runAsync(t, graph, func(Task) (Roles, error) {
		return Roles{
			Planner:  gatedAsker(plannerStarted, plannerRelease),
			Executor: instantAsker(),
			Verifier: instantAsker(),
		}, nil
	}, root.ID)
	waitChan(t, plannerStarted)
	before := graph.Snapshot()

	_, err = graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{ID: root.ID, Info: "root"}, {ID: "late", Info: "late source"}},
		Edges: []Edge{{From: "late:1:verifier", To: root.Planner.ID}},
	})
	if !errors.Is(err, ErrGraphBusy) {
		close(plannerRelease)
		t.Fatalf("ReplacePending() error = %v, want %v", err, ErrGraphBusy)
	}
	if after := graph.Snapshot(); !reflect.DeepEqual(after, before) {
		close(plannerRelease)
		t.Fatalf("graph changed after rejected mutation:\nafter  = %#v\nbefore = %#v", after, before)
	}

	_, err = graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{ID: root.ID, Info: "changed after start"}},
	})
	if !errors.Is(err, ErrGraphBusy) {
		close(plannerRelease)
		t.Fatalf("ReplacePending() task info error = %v, want %v", err, ErrGraphBusy)
	}
	if after := graph.Snapshot(); !reflect.DeepEqual(after, before) {
		close(plannerRelease)
		t.Fatalf("graph changed after rejected task info mutation:\nafter  = %#v\nbefore = %#v", after, before)
	}

	close(plannerRelease)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestGraphRunSameTaskStageWaitsForPreviousComplete(t *testing.T) {
	t.Parallel()

	graph := New()
	root := graph.AddTask()

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

func TestGraphRunAssembledReActSharesMemoryWithinTask(t *testing.T) {
	graph := New()
	store := ctxgraph.NewStore()
	task := graph.AddTask()
	store.Save(task.Env.ID, seededMemoryGraph())

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if response, ok, err := taskPackageOrganizerResponse(request); ok {
			return response, err
		}
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
		Stores{Memory: store},
		Assemble(
			Stores{Memory: store},
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
	store := ctxgraph.NewStore()
	store.Save("env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "bound"}},
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

	var original ctxgraph.Copy
	extra := agenttool.MemoryTools(func() ctxgraph.Copy {
		return original
	}, func(copy ctxgraph.Copy) error {
		original = copy
		return nil
	})
	roles, err := Assemble(
		Stores{Memory: store},
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
	if nodes := original.Graph.NodesInSubgraphs([]string{"bound"}); len(nodes) != 0 {
		t.Fatalf("assemble write leaked to the original memory callbacks: %#v", nodes)
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
			Stores{Memory: ctxgraph.NewStore()},
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
		if response, ok, err := taskPackageOrganizerResponse(request); ok {
			return response, err
		}
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

func taskPackageOrganizerResponse(request agent.Request) (agent.AssistantMessage, bool, error) {
	if !strings.Contains(request.SystemPrompt, "记忆子图整理 Agent") {
		return agent.AssistantMessage{}, false, nil
	}
	if hasToolResult(request.Messages) {
		return agent.AssistantMessage{Content: "organized"}, true, nil
	}
	query := firstUserContent(request.Messages)
	_, after, ok := strings.Cut(query, "目标子图 ID：")
	if !ok {
		return agent.AssistantMessage{}, true, fmt.Errorf("organizer query missing target: %q", query)
	}
	target, _, _ := strings.Cut(after, "\n")
	args, err := json.Marshal(map[string]any{
		"subgraph_id": strings.TrimSpace(target),
		"node_ids":    []string{"n1"},
	})
	if err != nil {
		return agent.AssistantMessage{}, true, err
	}
	return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
		ID: "prepare", Name: "memory_add_to_subgraph", Arguments: args,
	}}}, true, nil
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

func rolePromptAgents() agent.FileAgents {
	return agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "规划 Agent"},
		Executor: agent.FileAgent{SystemPrompt: "执行 Agent"},
		Verifier: agent.FileAgent{SystemPrompt: "核验 Agent"},
	}
}

func envMemoryAgents() agent.FileAgents {
	agents := rolePromptAgents()
	agents.Planner.Tools = []string{"memory_add_to_subgraph"}
	agents.Executor.Tools = []string{"memory_nodes_in"}
	agents.SubgraphOrganizer.SystemPrompt = "记忆子图整理 Agent"
	agents.SubgraphOrganizer.Tools = []string{"memory_add_to_subgraph"}
	return agents
}

func seededMemoryGraph() ctxgraph.Graph {
	return ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg"}},
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "shared",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	}
}

func setTaskInfo(t *testing.T, graph *Graph, id, info string) {
	t.Helper()
	graph.mu.Lock()
	defer graph.mu.Unlock()
	for i := range graph.tasks {
		if graph.tasks[i].ID == id {
			graph.tasks[i].Info = info
			return
		}
	}
	t.Fatalf("task %s missing", id)
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

func TestReplacePendingRejectsRunPolicyChangeOnStartedTask(t *testing.T) {
	t.Parallel()

	graph := New()
	snap, err := graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{Info: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := snap.Tasks[0]
	plannerStarted := make(chan struct{})
	plannerRelease := make(chan struct{})
	done := runAsync(t, graph, func(Task) (Roles, error) {
		return Roles{
			Planner:  gatedAsker(plannerStarted, plannerRelease),
			Executor: instantAsker(),
			Verifier: instantAsker(),
		}, nil
	}, root.ID)
	waitChan(t, plannerStarted)
	before := graph.Snapshot()

	_, err = graph.ReplacePending(context.Background(), PendingSubgraph{
		Tasks: []PendingTask{{Info: "root", RunPolicy: RunPolicyHeld}},
	})
	if !errors.Is(err, ErrGraphBusy) {
		close(plannerRelease)
		t.Fatalf("ReplacePending() run policy error = %v, want %v", err, ErrGraphBusy)
	}
	if after := graph.Snapshot(); !reflect.DeepEqual(after, before) {
		close(plannerRelease)
		t.Fatalf("graph changed after rejected run policy:\nafter  = %#v\nbefore = %#v", after, before)
	}

	close(plannerRelease)
	if err := waitErr(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
