package coordination_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

type replyFunc func(context.Context, string) (string, error)

func (f replyFunc) Ask(ctx context.Context, query string) (string, error) {
	return f(ctx, query)
}

func TestEdgesAllowIndependentConsumersAndRejectCycles(t *testing.T) {
	t.Parallel()
	graph := coordination.New()
	a, b, c := graph.AddTask(), graph.AddTask(), graph.AddTask()
	for _, edge := range []coordination.Edge{
		{From: a.Executor.ID, To: b.Planner.ID},
		{From: a.Executor.ID, To: c.Planner.ID},
	} {
		if err := graph.Connect(edge.From, edge.To); err != nil {
			t.Fatal(err)
		}
	}
	before, err := json.Marshal(graph.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Connect(b.Verifier.ID, a.Planner.ID); err == nil {
		t.Fatal("accepted a dependency cycle")
	}
	after, err := json.Marshal(graph.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected edge changed graph")
	}
	if got := graph.Incoming(c.Planner.ID); len(got) != 1 || got[0].ID != a.Executor.ID {
		t.Fatalf("consumer inputs = %#v", got)
	}
}

func TestIndependentTasksRunConcurrently(t *testing.T) {
	graph := coordination.New()
	a, b := graph.AddTask(), graph.AddTask()
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		planner := respond
		if task.ID == a.ID {
			planner = replyFunc(func(ctx context.Context, _ string) (string, error) {
				close(started)
				select {
				case <-release:
					return "released", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
		}
		return coordination.Roles{Planner: planner, Executor: respond, Verifier: respond}, nil
	}
	done := make(chan error, 1)
	go func() { _, err := graph.Run(ctx, a.ID, "a", stores, assemble); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err := graph.Run(ctx, b.ID, "b", stores, assemble)
	close(release)
	if err != nil {
		t.Errorf("independent task could not run: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentConsumersShareOneActivation(t *testing.T) {
	graph := coordination.New()
	task := graph.AddTask()
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var calls atomic.Int32
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(coordination.Task) (coordination.Roles, error) {
		return coordination.Roles{Planner: replyFunc(func(ctx context.Context, _ string) (string, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return "planned", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}), Executor: respond, Verifier: respond}, nil
	}
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	errs := make(chan error, 2)
	go func() { _, err := graph.Run(ctx, task.ID, "work", stores, assemble); errs <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() { _, err := graph.Run(ctx, task.ID, "work", stores, assemble); errs <- err }()
	select {
	case err := <-errs:
		t.Errorf("consumer returned before the shared activation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			if !t.Failed() {
				t.Fatal(ctx.Err())
			}
			return
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("planner ran %d times", calls.Load())
	}
}

func TestTaskCreationOrderDoesNotInheritState(t *testing.T) {
	t.Parallel()
	graph := coordination.New()
	first := graph.AddTask()
	second := graph.AddTask()
	files := vfs.NewStore(t.TempDir())
	stores := coordination.Stores{Memory: ctxgraph.NewStore(), Files: files}
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		return coordination.Roles{
			Planner: respond,
			Executor: replyFunc(func(context.Context, string) (string, error) {
				if task.ID == first.ID {
					return "created", files.View(task.Env.ID).Write("first.txt", []byte("first task only"))
				}
				if _, err := files.View(task.Env.ID).Read("first.txt"); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("independent task inherited first.txt: %v", err)
				}
				return "independent", nil
			}),
			Verifier: respond,
		}, nil
	}
	for _, task := range []coordination.Task{first, second} {
		if _, err := graph.Run(context.Background(), task.ID, "work", stores, assemble); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEdgeReadsFrozenRoleOutputBeforeSourceTaskFinishes(t *testing.T) {
	graph := coordination.New()
	source, target := graph.AddTask(), graph.AddTask()
	if err := graph.Connect(source.Executor.ID, target.Planner.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	files := vfs.NewStore(t.TempDir())
	stores := coordination.Stores{Memory: ctxgraph.NewStore(), Files: files}
	verifying, release := make(chan struct{}), make(chan struct{})
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := coordination.Roles{Planner: respond, Executor: respond, Verifier: respond}
		if task.ID == source.ID {
			roles.Executor = replyFunc(func(context.Context, string) (string, error) {
				return "published", files.View(task.Env.ID).Write("state.txt", []byte("executor snapshot"))
			})
			roles.Verifier = replyFunc(func(ctx context.Context, _ string) (string, error) {
				if err := files.View(task.Env.ID).Write("state.txt", []byte("later mutation")); err != nil {
					return "", err
				}
				close(verifying)
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-release:
					return "verified", nil
				}
			})
		} else {
			roles.Executor = replyFunc(func(context.Context, string) (string, error) {
				data, err := files.View(task.Env.ID).Read("state.txt")
				if err != nil || string(data) != "executor snapshot" {
					t.Errorf("inherited file = %q, %v", data, err)
				}
				return "consumed", nil
			})
		}
		return roles, nil
	}
	done := make(chan error, 1)
	go func() { _, err := graph.Run(ctx, source.ID, "produce", stores, assemble); done <- err }()
	select {
	case <-verifying:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err := graph.Run(ctx, target.ID, "consume", stores, assemble)
	close(release)
	if err != nil {
		t.Error(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPersistentTaskContinuesFromPairedSnapshotAfterRestart(t *testing.T) {
	base, state := t.TempDir(), t.TempDir()
	graphPath := filepath.Join(state, "graph.json")
	memoryPath := filepath.Join(state, "memory.json")
	graph, err := coordination.OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := graph.ReplacePending(context.Background(), coordination.PendingSubgraph{Tasks: []coordination.PendingTask{{ID: "thread", Info: "keep state", Persistent: true}}})
	if err != nil {
		t.Fatal(err)
	}
	task := snapshot.Tasks[0]
	files, err := vfs.NewPersistentStore(base, filepath.Join(state, "files"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err := ctxgraph.OpenStore(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		return coordination.Roles{Planner: respond, Executor: replyFunc(func(context.Context, string) (string, error) {
			if task.Activation == 1 {
				return "created", files.View(task.Env.ID).Write("saved.txt", []byte("retained"))
			}
			data, err := files.View(task.Env.ID).Read("saved.txt")
			if err != nil || string(data) != "retained" {
				t.Errorf("restored file = %q, %v", data, err)
			}
			return "continued", nil
		}), Verifier: respond}, nil
	}
	if _, err := graph.Run(context.Background(), task.ID, "first", coordination.Stores{Memory: memory, Files: files}, assemble); err != nil {
		t.Fatal(err)
	}
	if current, _ := graph.Task(task.ID); current.Outcome != coordination.OutcomeIdle {
		t.Fatalf("persistent outcome = %s", current.Outcome)
	}
	graph, err = coordination.OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	files, err = vfs.NewPersistentStore(base, filepath.Join(state, "files"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err = ctxgraph.OpenStore(memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := graph.Continue(task.ID, "continue")
	if err != nil {
		t.Fatal(err)
	}
	if continued.Activation != 2 || continued.Env.ID == task.Env.ID {
		t.Fatalf("continuation identity = %#v", continued)
	}
	if _, err := graph.Run(context.Background(), task.ID, "continue", coordination.Stores{Memory: memory, Files: files}, assemble); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedRoleIsNotReplayedAfterOutputCommitFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "graph")
	graph, err := coordination.OpenGraph(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	task := graph.AddTask()
	progress, err := coordination.NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	stores := coordination.Stores{Memory: ctxgraph.NewStore()}
	calls := 0
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(coordination.Task) (coordination.Roles, error) {
		return coordination.Roles{Planner: respond, Executor: respond, Verifier: replyFunc(func(context.Context, string) (string, error) {
			calls++
			if calls == 1 {
				if err := os.Rename(dir, dir+"-saved"); err != nil {
					return "", err
				}
			}
			return "immutable report", nil
		})}, nil
	}
	if _, err := graph.Run(context.Background(), task.ID, "work", stores, assemble); err == nil {
		t.Fatal("missing graph store accepted output commit")
	}
	if err := os.Rename(dir+"-saved", dir); err != nil {
		t.Fatal(err)
	}
	if output, err := graph.Run(context.Background(), task.ID, "work", stores, assemble); err != nil || output != "immutable report" {
		t.Fatalf("retry = %q, %v", output, err)
	}
	if calls != 1 {
		t.Fatalf("completed verifier replayed %d times", calls)
	}
}

func TestInputResumesMemoryAfterFilesWithoutLeakingCandidateFacts(t *testing.T) {
	graph := coordination.New()
	a, b, target := graph.AddTask(), graph.AddTask(), graph.AddTask()
	for _, source := range []coordination.Task{a, b} {
		if err := graph.Connect(source.Verifier.ID, target.Planner.ID); err != nil {
			t.Fatal(err)
		}
	}
	progress, err := coordination.NewDirProgressStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	files := vfs.NewStore(t.TempDir())
	memory := ctxgraph.NewStore()
	stores := coordination.Stores{Files: files, Memory: memory}
	inputTool := graph.HelpTools(nil)["input"]
	fileCalls, memoryCalls, roleCalls := 0, 0, 0
	retry := errors.New("organizer interrupted")
	respond := replyFunc(func(context.Context, string) (string, error) { return "done", nil })
	assemble := func(task coordination.Task) (coordination.Roles, error) {
		roles := coordination.Roles{Planner: respond, Executor: respond, Verifier: respond}
		if task.ID != target.ID {
			roles.Executor = replyFunc(func(context.Context, string) (string, error) {
				value := "chosen"
				if task.ID == b.ID {
					value = "rejected"
				}
				if err := files.View(task.Env.ID).Write("value.txt", []byte(value)); err != nil {
					return "", err
				}
				return value, memory.Save(task.Env.ID, ctxgraph.Graph{Subgraphs: []ctxgraph.Subgraph{{ID: "facts"}}, Nodes: []ctxgraph.Node{
					{ID: "common", Kind: ctxgraph.NodeKindDirective, Statement: "keep evidence", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"facts"}},
					{ID: "implementation", Kind: ctxgraph.NodeKindFact, Statement: value, Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"facts"}},
				}})
			})
			return roles, nil
		}
		roles.ResolveInput = func(ctx context.Context, node coordination.Node, input coordination.InputProgress) error {
			fileCalls++
			bound := agenttool.Bind(memory, input.TargetID, []agenttool.Tool{inputTool})[0]
			for _, args := range []map[string]any{
				{"action": "apply", "session_id": input.ID, "source_id": a.Verifier.ID, "all": true},
				{"action": "discard", "session_id": input.ID, "source_id": b.Verifier.ID, "reason": "unselected implementation"},
				{"action": "finish", "session_id": input.ID, "reason": "chosen file state verified"},
			} {
				data, err := json.Marshal(args)
				if err != nil {
					return err
				}
				if _, err := bound.Execute(agenttool.WithAgentID(ctx, node.ID), agenttool.Call{ID: "decision", Name: "input", Arguments: data}); err != nil {
					return err
				}
			}
			return nil
		}
		roles.OrganizeMemory = func(_ context.Context, request agent.InputMemoryRequest) (ctxgraph.Graph, error) {
			memoryCalls++
			if fileCalls != 1 {
				t.Errorf("file stage ran %d times", fileCalls)
			}
			data, err := files.View(request.FilesRef).Read("value.txt")
			if err != nil || string(data) != "chosen" {
				t.Errorf("organizer file evidence = %q, %v", data, err)
			}
			if !strings.Contains(request.Evidence, "chosen") {
				t.Error("final file evidence absent")
			}
			for _, node := range memory.Load(target.Env.ID).Nodes {
				if node.ID == "implementation" {
					t.Error("candidate fact entered live target before ready")
				}
			}
			if memoryCalls == 1 {
				return ctxgraph.Graph{}, retry
			}
			common := ctxgraph.PartitionInputs(request.Sources).Common
			common.Nodes = append(common.Nodes, ctxgraph.Node{ID: "accepted", Kind: ctxgraph.NodeKindFact, Statement: "chosen", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"facts"}, SourceRefs: []string{request.FilesRef}})
			return common, nil
		}
		roles.Planner = replyFunc(func(context.Context, string) (string, error) {
			roleCalls++
			for _, node := range memory.Load(target.Env.ID).Nodes {
				if node.Statement == "rejected" {
					t.Error("rejected fact leaked into role")
				}
			}
			return "ready", nil
		})
		return roles, nil
	}
	if _, err := graph.Run(context.Background(), target.ID, "combine", stores, assemble); !errors.Is(err, retry) {
		t.Fatalf("first attempt = %v", err)
	}
	if roleCalls != 0 {
		t.Fatal("role started before ready")
	}
	if _, err := graph.Run(context.Background(), target.ID, "combine", stores, assemble); err != nil {
		t.Fatal(err)
	}
	if fileCalls != 1 || memoryCalls != 2 || roleCalls != 1 {
		t.Fatalf("stages = files:%d memory:%d role:%d", fileCalls, memoryCalls, roleCalls)
	}
	output, _ := graph.Output(b.Verifier.ID)
	if memory.Load(output.MemoryRef).Nodes[1].Status != ctxgraph.NodeStatusAccepted {
		t.Fatal("source graph was rewritten")
	}
}
