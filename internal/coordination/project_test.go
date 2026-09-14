package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestRealDirectoryCanOnlyBeSelectedForANewTask(t *testing.T) {
	graph := New()
	old := graph.AddTask()
	_, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{ID: old.ID, Info: "existing", RealDirectory: true}}})
	if err == nil {
		t.Fatal("converted an existing task to the real directory")
	}
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{Info: "debug real files", RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || !snap.Tasks[0].RealDirectory {
		t.Fatalf("created tasks=%+v", snap.Tasks)
	}
}

func TestRealDirectoryTaskUsesPreparedInputAndRunsOnTheProject(t *testing.T) {
	graph := New()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "file"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := vfs.NewPersistentStore(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files, Exec: tmexec.New(tmexec.Config{ExternalSandbox: true})}
	source := graph.AddTask()
	var bound string
	resolvedProject := false
	assemble := func(task Task) (Roles, error) {
		return Roles{
			ResolveInput: func(ctx context.Context, node Node, input InputProgress) error {
				if len(input.Sources) != 2 {
					t.Fatalf("sources=%+v", input.Sources)
				}
				project := input.Sources[1]
				got, err := files.View(project.FilesRef).Read("local")
				if err != nil || string(got) != "existing project" {
					t.Fatalf("real directory input=%q,%v", got, err)
				}
				tool := agenttool.Bind(stores.Memory, input.TargetID, []agenttool.Tool{graph.HelpTools(nil)["input"]})[0]
				for _, args := range []map[string]any{
					{"action": "apply", "session_id": input.ID, "source_id": input.Sources[0].ID, "all": true},
					{"action": "apply", "session_id": input.ID, "source_id": project.ID, "paths": []string{"local"}, "strategy": "replace", "reason": "retain real directory local file"},
					{"action": "discard", "session_id": input.ID, "source_id": project.ID, "reason": "keep local; use incoming implementation"},
					{"action": "finish", "session_id": input.ID, "reason": "checked merged files"},
				} {
					data, _ := json.Marshal(args)
					if _, err := tool.Execute(agenttool.WithAgentID(ctx, node.ID), agenttool.Call{ID: "decision", Name: "input", Arguments: data}); err != nil {
						return err
					}
				}
				resolvedProject = true
				return nil
			},
			Planner: instantAsker(),
			Verifier: askerFunc(func(ctx context.Context, _ string) (string, error) {
				if !task.RealDirectory {
					return "source verified", nil
				}
				live, err := files.Materialize(bound)
				if err != nil || live != base {
					t.Fatalf("verifier workspace=%q,%v, want real directory", live, err)
				}
				result, err := stores.Exec.View(bound, files).Run(ctx, env.Cmd{Command: `test "$(cat file)" = debugged && printf pass > acceptance.txt`})
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("acceptance=%+v,%v", result, err)
				}
				return "real directory accepted", nil
			}),
			Executor: askerFunc(func(_ context.Context, _ string) (string, error) {
				if task.ID == source.ID {
					// Simulate an external edit while the consumer waits for us.
					// Its project source must already have been captured.
					if err := os.WriteFile(filepath.Join(base, "local"), []byte("changed while waiting"), 0600); err != nil {
						return "", err
					}
					return "source", files.View(bound).Write("file", []byte("prepared"))
				}
				live, err := files.Materialize(bound)
				if err != nil {
					return "", err
				}
				if live != base {
					t.Fatalf("executor ran in %q, want %q", live, base)
				}
				got, err := files.View(bound).Read("file")
				if err != nil || string(got) != "prepared" {
					t.Fatalf("input=%q,%v", got, err)
				}
				return "fixed", files.View(bound).Write("file", []byte("debugged"))
			}),
			bind: func(_, _, workspace string) error { bound = workspace; return nil },
		}, nil
	}
	if err := os.WriteFile(filepath.Join(base, "local"), []byte("existing project"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "drop"), []byte("discard me"), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{ID: source.ID}, {ID: "task-2", Info: "debug", RealDirectory: true}}, Edges: []Edge{{From: source.Verifier.ID, To: "task-2:1:planner"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 2 {
		t.Fatalf("tasks=%+v", snap.Tasks)
	}
	if _, err := graph.Run(t.Context(), "task-2", "", stores, assemble); err != nil {
		t.Fatal(err)
	}
	if !resolvedProject {
		t.Fatal("real directory was not offered to the input resolver")
	}
	if got, err := os.ReadFile(filepath.Join(base, "acceptance.txt")); err != nil || string(got) != "pass" {
		t.Fatalf("acceptance did not run in project: %q,%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "drop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected real input survived: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "local")); err != nil || string(got) != "existing project" {
		t.Fatalf("merged local=%q,%v", got, err)
	}
	got, err := os.ReadFile(filepath.Join(base, "file"))
	if err != nil || string(got) != "debugged" {
		t.Fatalf("real result=%q,%v", got, err)
	}
	output, _ := graph.Output(source.Verifier.ID)
	got, err = files.View(output.FilesRef).Read("file")
	if err != nil || string(got) != "prepared" {
		t.Fatalf("source mutated=%q,%v", got, err)
	}
}

func TestRealDirectoryOwnershipPersistsAndRejectsConcurrentTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{Info: "real", RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	owner := first.Tasks[0]
	_, err = graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{ID: owner.ID}, {Info: "second real", RealDirectory: true}}})
	if err == nil {
		t.Fatal("two active tasks can own the real directory")
	}
	reopened, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	state := reopened.Snapshot()
	if state.ProjectTaskID != owner.ID || len(state.Tasks) != 1 || !state.Tasks[0].RealDirectory {
		t.Fatalf("recovered ownership=%+v", state)
	}
}

func TestRealDirectoryPreviousOwnerCannotContinue(t *testing.T) {
	graph := New()
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{Info: "watch", Persistent: true, RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	first := snap.Tasks[0]
	if err := graph.commitOutput(Output{Node: first.Verifier, FilesRef: "files", MemoryRef: "memory"}); err != nil {
		t.Fatal(err)
	}
	graph.tasks[0].Outcome = OutcomeIdle
	if _, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{ID: first.ID}, {Info: "new owner", RealDirectory: true}}}); err != nil {
		t.Fatal(err)
	}
	before := graph.Snapshot()
	if _, err := graph.Continue(first.ID, "continue watching"); err == nil {
		t.Fatal("former owner continued")
	}
	if !reflect.DeepEqual(before, graph.Snapshot()) {
		t.Fatal("rejected continuation changed graph")
	}
}

func TestRealDirectoryRecoveryKeepsEditsWithoutReinstallingInput(t *testing.T) {
	base, state := t.TempDir(), t.TempDir()
	graphPath := filepath.Join(state, "graph.json")
	graph, err := OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := NewDirProgressStore(filepath.Join(state, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	files, err := vfs.NewPersistentStore(base, filepath.Join(state, "files"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err := ctxgraph.OpenStore(filepath.Join(state, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := graph.ReplacePending(t.Context(), PendingSubgraph{Tasks: []PendingTask{{Info: "debug", RealDirectory: true}}})
	if err != nil {
		t.Fatal(err)
	}
	task := snap.Tasks[0]
	var workspace string
	crashed := errors.New("interrupted executor")
	_, err = graph.Run(t.Context(), task.ID, "", Stores{Memory: memory, Files: files}, func(Task) (Roles, error) {
		return Roles{Planner: instantAsker(), Verifier: instantAsker(),
			bind: func(_, _, id string) error { workspace = id; return nil },
			Executor: askerFunc(func(context.Context, string) (string, error) {
				if err := files.View(workspace).Write("debug.txt", []byte("before restart")); err != nil {
					return "", err
				}
				return "", crashed
			}),
		}, nil
	})
	if !errors.Is(err, crashed) {
		t.Fatalf("run error=%v", err)
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	// A restart may also adopt a newer floor. The committed planner output must
	// remain fixed, while the interrupted executor resumes its real files.
	graph, err = OpenGraph(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)
	files, err = vfs.NewPersistentStore(base, filepath.Join(state, "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	memory, err = ctxgraph.OpenStore(filepath.Join(state, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = graph.Run(t.Context(), task.ID, "", Stores{Memory: memory, Files: files}, func(Task) (Roles, error) {
		return Roles{Planner: askerFunc(func(context.Context, string) (string, error) { t.Error("replayed planner"); return "", nil }), Verifier: instantAsker(),
			bind: func(_, _, id string) error { workspace = id; return nil },
			Executor: askerFunc(func(context.Context, string) (string, error) {
				got, err := files.View(workspace).Read("debug.txt")
				if err != nil || string(got) != "before restart" {
					t.Fatalf("resumed edit=%q,%v", got, err)
				}
				return "done", files.View(workspace).Write("debug.txt", []byte("finished"))
			}),
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(base, "debug.txt"))
	if err != nil || string(got) != "finished" {
		t.Fatalf("real result=%q,%v", got, err)
	}
	output, _ := graph.Output(task.Planner.ID)
	if err := files.Restore(output.FilesRef); err != nil {
		t.Fatal(err)
	}
	if _, err := files.View(output.FilesRef).Read("debug.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planner archive changed: %v", err)
	}
}
