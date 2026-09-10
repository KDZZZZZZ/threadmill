package coordination_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestOwnerReportPersistsBeforeTaskOutcome(t *testing.T) {
	graph := coordination.New()
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	reportErr := errors.New("report projection unavailable")
	for range 100 {
		task := graph.AddTask()
		_, err := graph.RunWithReport(context.Background(), task.ID, "work", stores,
			func(coordination.Task) (coordination.Roles, error) { return reviewRoles(), nil },
			func(reported coordination.Task, _ string, taskErr error) error {
				current, _ := graph.Task(task.ID)
				if current.Outcome != coordination.OutcomeActive {
					t.Errorf("published outcome %s before required report persisted", current.Outcome)
				}
				if reported.Outcome != coordination.OutcomeDone || taskErr != nil {
					t.Errorf("report = %+v, %v", reported, taskErr)
				}
				return reportErr
			},
		)
		if !errors.Is(err, reportErr) {
			t.Fatalf("report failure = %v", err)
		}
		if current, _ := graph.Task(task.ID); current.Outcome != coordination.OutcomeActive {
			t.Fatalf("failed report left task %s", current.Outcome)
		}
	}
}

func TestAttachedReportsFinishBeforeSharedActivationCloses(t *testing.T) {
	graph := coordination.New()
	task := graph.AddTask()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(func() { cancel(); graph.WaitRuns() })
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	firstReporting, secondReporting := make(chan struct{}), make(chan struct{})
	releaseFirst, releaseSecond := make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	assemble := func(coordination.Task) (coordination.Roles, error) { return reviewRoles(), nil }
	go func() {
		_, err := graph.RunWithReport(ctx, task.ID, "work", stores, assemble,
			func(coordination.Task, string, error) error {
				close(firstReporting)
				select {
				case <-releaseFirst:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		results <- err
	}()
	reviewAwait(t, ctx, firstReporting)
	go func() {
		_, err := graph.RunWithReport(ctx, task.ID, "work", stores, assemble,
			func(coordination.Task, string, error) error {
				close(secondReporting)
				select {
				case <-releaseSecond:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		results <- err
	}()
	select {
	case err := <-results:
		t.Fatalf("shared run returned during required report: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	reviewAwait(t, ctx, secondReporting)
	if current, _ := graph.Task(task.ID); current.Outcome != coordination.OutcomeActive {
		t.Errorf("shared task became %s before attached report persisted", current.Outcome)
	}
	close(releaseSecond)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestInputResolverFailureReapsItsWorkspaceProcesses(t *testing.T) {
	graph := coordination.New()
	a, b, target := reviewInputGraph(t, graph)
	files := vfs.NewStore(t.TempDir())
	exec := tmexec.New(tmexec.Config{Slots: 1, ExternalSandbox: true})
	stores := coordination.Stores{Memory: ctxgraph.NewStore(), Files: files, Exec: exec}
	var workspace string
	t.Cleanup(func() {
		_ = exec.Reap(workspace)
		_ = files.Close()
	})
	failed := errors.New("resolver interrupted before finish")
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := reviewSourceRoles(task, target, stores)
		if task.ID == target.ID {
			roles.ResolveInput = func(ctx context.Context, _ coordination.Node, input coordination.InputProgress) error {
				workspace = input.TargetID
				result, err := exec.View(workspace, files).Run(ctx, env.Cmd{Command: "sleep 30 &"})
				if err != nil || result.ExitCode != 0 {
					return fmt.Errorf("start background resolver command: %+v, %w", result, err)
				}
				if exec.Stats().TrackedProcessGroups != 1 {
					return errors.New("fixture did not retain background resolver process")
				}
				return failed
			}
		}
		return roles, nil
	}
	for _, task := range []coordination.Task{a, b} {
		if _, err := graph.Run(context.Background(), task.ID, "produce", stores, assemble); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := graph.Run(context.Background(), target.ID, "combine", stores, assemble); !errors.Is(err, failed) {
		t.Fatalf("resolver failure = %v", err)
	}
	graph.WaitRuns()
	if got := exec.Stats().TrackedProcessGroups; got != 0 {
		t.Errorf("resolver failure retained %d background process group(s)", got)
	}
	if got := files.Stats().LiveDirs; got != 0 {
		t.Errorf("resolver failure retained %d live workspace(s)", got)
	}
}

func TestInputReadySaveRetryCompletesWorkspaceCleanup(t *testing.T) {
	graph := coordination.New()
	a, b, target := reviewInputGraph(t, graph)
	progress, err := coordination.NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failed := errors.New("ready checkpoint unavailable")
	graph.SetProgressStore(&failReviewReadyStore{ProgressStore: progress, nodeID: target.Planner.ID, failure: failed})
	files := vfs.NewStore(t.TempDir())
	stores := coordination.Stores{Memory: ctxgraph.NewStore(), Files: files}
	t.Cleanup(func() { _ = files.Close() })
	tool := graph.HelpTools(nil)["input"]
	var workspaces []string
	var resolverCalls int
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := reviewSourceRoles(task, target, stores)
		if task.ID == target.ID {
			roles.ResolveInput = func(ctx context.Context, node coordination.Node, input coordination.InputProgress) error {
				resolverCalls++
				workspaces = append(workspaces, input.TargetID)
				for _, source := range input.Sources {
					workspaces = append(workspaces, source.EnvID)
				}
				bound := agenttool.Bind(stores.Memory, input.TargetID, []agenttool.Tool{tool})[0]
				for _, args := range []map[string]any{
					{"action": "apply", "session_id": input.ID, "source_id": a.Verifier.ID, "all": true},
					{"action": "discard", "session_id": input.ID, "source_id": b.Verifier.ID, "reason": "unselected file"},
					{"action": "finish", "session_id": input.ID, "reason": "file choice complete"},
				} {
					data, err := json.Marshal(args)
					if err != nil {
						return err
					}
					if _, err := bound.Execute(agenttool.WithAgentID(ctx, node.ID), agenttool.Call{ID: "decide", Name: "input", Arguments: data}); err != nil {
						return err
					}
				}
				return nil
			}
		}
		return roles, nil
	}
	for _, task := range []coordination.Task{a, b} {
		if _, err := graph.Run(context.Background(), task.ID, "produce", stores, assemble); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := graph.Run(context.Background(), target.ID, "combine", stores, assemble); !errors.Is(err, failed) {
		t.Fatalf("first ready save = %v", err)
	}
	if _, err := graph.Run(context.Background(), target.ID, "combine", stores, assemble); err != nil {
		t.Fatalf("ready retry = %v", err)
	}
	if resolverCalls != 1 {
		t.Fatalf("successful file decisions replayed %d times", resolverCalls)
	}
	for _, workspace := range workspaces {
		if err := files.Restore(workspace); !errors.Is(err, vfs.ErrUnknownEnvironment) {
			t.Errorf("ready retry retained disposable workspace %q: %v", workspace, err)
		}
	}
}

func TestStartedRoleRestoresItsFilesAfterStoreReopen(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "unmaterialized input"
		if edited {
			name = "retained role edits"
		}
		t.Run(name, func(t *testing.T) {
			base, state := t.TempDir(), t.TempDir()
			graphPath := filepath.Join(state, "graph.json")
			memoryPath := filepath.Join(state, "memory.json")
			filesPath := filepath.Join(state, "files")
			progressPath := filepath.Join(state, "progress")
			graph, err := coordination.OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			source, target := graph.AddTask(), graph.AddTask()
			if err := graph.Connect(source.Verifier.ID, target.Planner.ID); err != nil {
				t.Fatal(err)
			}
			progress, err := coordination.NewDirProgressStore(progressPath)
			if err != nil {
				t.Fatal(err)
			}
			graph.SetProgressStore(progress)
			memory, err := ctxgraph.OpenStore(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			files, err := vfs.NewPersistentStore(base, filesPath)
			if err != nil {
				t.Fatal(err)
			}
			stores := coordination.Stores{Memory: memory, Files: files}
			assemble := func(task coordination.Task) (coordination.Roles, error) {
				roles := reviewRoles()
				if task.ID == source.ID {
					roles.Executor = replyFunc(func(context.Context, string) (string, error) {
						return "source", files.View(task.Env.ID).Write("inherited.txt", []byte("source snapshot"))
					})
				} else {
					roles.Planner = replyFunc(func(context.Context, string) (string, error) {
						if edited {
							if err := files.View(task.Env.ID).Write("inherited.txt", []byte("role edit")); err != nil {
								return "", err
							}
						}
						if err := memory.Save(task.Env.ID, ctxgraph.Graph{Nodes: []ctxgraph.Node{{
							ID: "role-note", Kind: ctxgraph.NodeKindDirective, Statement: "preserve current role memory", Status: ctxgraph.NodeStatusAccepted,
						}}}); err != nil {
							return "", err
						}
						return "", context.Canceled
					})
				}
				return roles, nil
			}
			if _, err := graph.Run(context.Background(), target.ID, "consume source", stores, assemble); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted role = %v", err)
			}
			graph.WaitRuns()
			if err := files.Close(); err != nil {
				t.Fatal(err)
			}
			restoredGraph, err := coordination.OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			restoredGraph.SetProgressStore(progress)
			restoredMemory, err := ctxgraph.OpenStore(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			restoredFiles, err := vfs.NewPersistentStore(base, filesPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restoredFiles.Close() })
			_, err = restoredGraph.Run(context.Background(), target.ID, "resume", coordination.Stores{Memory: restoredMemory, Files: restoredFiles},
				func(task coordination.Task) (coordination.Roles, error) {
					roles := reviewRoles()
					roles.Planner = replyFunc(func(context.Context, string) (string, error) {
						want := "source snapshot"
						if edited {
							want = "role edit"
						}
						got, err := restoredFiles.View(task.Env.ID).Read("inherited.txt")
						if err != nil || string(got) != want {
							return "", fmt.Errorf("restored role file = %q, %v; want %q", got, err, want)
						}
						current := restoredMemory.Load(task.Env.ID)
						if len(current.Nodes) != 1 || current.Nodes[0].ID != "role-note" {
							return "", fmt.Errorf("restored role memory was reset: %+v", current.Nodes)
						}
						return "resumed", nil
					})
					return roles, nil
				})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHeldSourceBlocksOnlyItsDependentsUntilReleased(t *testing.T) {
	graph := coordination.New()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(func() { cancel(); graph.WaitRuns() })
	graph.SetRunContext(ctx)
	pending := coordination.PendingSubgraph{
		Tasks: []coordination.PendingTask{
			{ID: "source", Info: "produce input", RunPolicy: coordination.RunPolicyHeld},
			{ID: "target", Info: "consume source"},
			{ID: "independent", Info: "unrelated work"},
		},
		Edges: []coordination.Edge{{From: "source:1:verifier", To: "target:1:planner"}},
	}
	if _, err := graph.ReplacePending(ctx, pending); err != nil {
		t.Fatal(err)
	}
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	sourceStarted := make(chan struct{})
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := reviewRoles()
		if task.ID == "source" {
			roles.Planner = replyFunc(func(context.Context, string) (string, error) {
				close(sourceStarted)
				return "source ready", nil
			})
		}
		return roles, nil
	}
	targetDone := make(chan error, 1)
	go func() {
		_, err := graph.Run(ctx, "target", "consume", stores, assemble)
		targetDone <- err
	}()
	if _, err := graph.Run(ctx, "independent", "unrelated", stores, assemble); err != nil {
		t.Fatalf("unrelated task was blocked by held source: %v", err)
	}
	if task, _ := graph.Task("independent"); task.Outcome != coordination.OutcomeDone {
		t.Fatalf("unrelated task outcome = %s", task.Outcome)
	}
	select {
	case <-sourceStarted:
		t.Fatal("dependent task started its held source")
	case err := <-targetDone:
		t.Fatalf("dependent task completed before its source was released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	pending.Tasks[0].RunPolicy = coordination.RunPolicyEnabled
	if _, err := graph.ReplacePending(ctx, pending); err != nil {
		t.Fatalf("release held source: %v", err)
	}
	reviewAwait(t, ctx, sourceStarted)
	select {
	case err := <-targetDone:
		if err != nil {
			t.Fatalf("dependent task did not resume after release: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestStartedInputRemainsFrozenAfterGraphReopen(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := "rewire"
		if remove {
			name = "remove"
		}
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			graphPath := filepath.Join(state, "graph.json")
			graph, err := coordination.OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			progress, err := coordination.NewDirProgressStore(filepath.Join(state, "progress"))
			if err != nil {
				t.Fatal(err)
			}
			graph.SetProgressStore(progress)
			pending := coordination.PendingSubgraph{
				Tasks: []coordination.PendingTask{
					{ID: "source", Info: "original input"},
					{ID: "alternative", Info: "different input"},
					{ID: "target", Info: "consume original input"},
				},
				Edges: []coordination.Edge{{From: "source:1:verifier", To: "target:1:planner"}},
			}
			if _, err := graph.ReplacePending(context.Background(), pending); err != nil {
				t.Fatal(err)
			}
			reportErr := errors.New("report storage unavailable")
			_, err = graph.RunWithReport(context.Background(), "target", "begin", coordination.Stores{Memory: ctxgraph.NewStore()},
				func(task coordination.Task) (coordination.Roles, error) {
					roles := reviewRoles()
					if task.ID == "target" {
						roles.Planner = replyFunc(func(context.Context, string) (string, error) {
							return "", context.Canceled
						})
					}
					return roles, nil
				},
				func(coordination.Task, string, error) error { return reportErr },
			)
			if !errors.Is(err, context.Canceled) || !errors.Is(err, reportErr) {
				t.Fatalf("interrupted role with failed report = %v", err)
			}
			graph.WaitRuns()
			if task, _ := graph.Task("target"); task.Outcome != coordination.OutcomeActive {
				t.Fatalf("failed report must retain active task, got %s", task.Outcome)
			}
			if _, ok := graph.Output("target:1:planner"); ok {
				t.Fatal("fixture must interrupt the role before its output commits")
			}
			restored, err := coordination.OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			restored.SetProgressStore(progress)
			before := restored.Snapshot()
			if remove {
				pending.Tasks = pending.Tasks[:2]
				pending.Edges = nil
			} else {
				pending.Edges[0].From = "alternative:1:verifier"
			}
			if _, err := restored.ReplacePending(context.Background(), pending); !errors.Is(err, coordination.ErrGraphBusy) {
				t.Errorf("%s accepted a persisted input that already started: %v", name, err)
			}
			if after := restored.Snapshot(); !reflect.DeepEqual(after, before) {
				t.Error("rejected edit changed the persisted graph")
			}
		})
	}
}

func TestSingleConsumerRunStartsAllSourcesConcurrentlyInInputOrder(t *testing.T) {
	graph := coordination.New()
	a, b, target := reviewInputGraph(t, graph)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(func() { cancel(); graph.WaitRuns() })
	graph.SetRunContext(ctx)
	progress, err := coordination.NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	aStarted, bStarted := make(chan struct{}), make(chan struct{})
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := reviewRoles()
		if task.ID == target.ID {
			return roles, nil
		}
		roles.Planner = replyFunc(func(ctx context.Context, _ string) (string, error) {
			own, other := aStarted, bStarted
			if task.ID == b.ID {
				own, other = bStarted, aStarted
			}
			close(own)
			select {
			case <-other:
				return "both source planners started", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		roles.Verifier = replyFunc(func(context.Context, string) (string, error) {
			return task.ID + " verified", nil
		})
		return roles, nil
	}
	if _, err := graph.Run(ctx, target.ID, "combine", coordination.Stores{Memory: ctxgraph.NewStore()}, assemble); err != nil {
		t.Fatalf("single consumer could not start both source planners before waiting: %v", err)
	}
	state, ok, err := progress.Load(target.Env.ID)
	if err != nil || !ok {
		t.Fatalf("consumer input record = %+v, %v, %v", state, ok, err)
	}
	for _, input := range state.Inputs {
		if input.NodeID != target.Planner.ID {
			continue
		}
		if len(input.Sources) != 2 {
			t.Fatalf("consumer sources = %+v", input.Sources)
		}
		for i, source := range []coordination.Task{a, b} {
			got := input.Sources[i]
			if got.ID != source.Verifier.ID || got.Output != source.ID+" verified" {
				t.Errorf("source %d = %+v; want %s with its corresponding report", i, got, source.Verifier.ID)
			}
		}
		return
	}
	t.Fatal("consumer planner has no recorded input batch")
}

type failReviewReadyStore struct {
	coordination.ProgressStore
	nodeID  string
	failure error
	failed  atomic.Bool
}

func (s *failReviewReadyStore) Save(taskID string, progress coordination.TaskProgress) error {
	for _, input := range progress.Inputs {
		if input.NodeID == s.nodeID && input.Phase == "ready" && s.failed.CompareAndSwap(false, true) {
			return s.failure
		}
	}
	return s.ProgressStore.Save(taskID, progress)
}

func reviewInputGraph(t *testing.T, graph *coordination.Graph) (coordination.Task, coordination.Task, coordination.Task) {
	t.Helper()
	a, b, target := graph.AddTask(), graph.AddTask(), graph.AddTask()
	for _, source := range []coordination.Task{a, b} {
		if err := graph.Connect(source.Verifier.ID, target.Planner.ID); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, target
}

func reviewSourceRoles(task, target coordination.Task, stores coordination.Stores) coordination.Roles {
	roles := reviewRoles()
	if task.ID != target.ID {
		roles.Executor = replyFunc(func(context.Context, string) (string, error) {
			return "produced", stores.Files.View(task.Env.ID).Write("choice.txt", []byte(task.ID))
		})
	}
	return roles
}

func reviewRoles() coordination.Roles {
	reply := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	return coordination.Roles{Planner: reply, Executor: reply, Verifier: reply}
}

func reviewAwait(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
