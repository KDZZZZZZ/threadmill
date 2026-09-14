package env

import (
	"context"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestOpenSetsIDAndMemory(t *testing.T) {
	t.Parallel()

	view := stubMemory{}
	got := Open("env-a", view)
	if got.ID != "env-a" {
		t.Fatalf("ID = %q, want env-a", got.ID)
	}
	if got.Memory != view {
		t.Fatal("Memory was not the view passed to Open")
	}
	if got.Files != nil {
		t.Fatal("Files = not nil, want zero value nil")
	}
	if got.Exec != nil {
		t.Fatal("Exec = not nil, want zero value nil")
	}
}

func TestWithFilesSetsFiles(t *testing.T) {
	t.Parallel()

	view := stubMemory{}
	files := stubFiles{}
	got := Open("env-a", view).WithFiles(files)
	if got.ID != "env-a" {
		t.Fatalf("ID = %q, want env-a", got.ID)
	}
	if got.Memory != view {
		t.Fatal("Memory was not the view passed to Open")
	}
	if got.Files != files {
		t.Fatal("Files was not the view passed to WithFiles")
	}
	if got.Exec != nil {
		t.Fatal("WithFiles set Exec")
	}
}

func TestWithExecSetsExec(t *testing.T) {
	t.Parallel()

	view := stubMemory{}
	execView := stubExec{}
	got := Open("env-a", view).WithExec(execView)
	if got.ID != "env-a" {
		t.Fatalf("ID = %q, want env-a", got.ID)
	}
	if got.Memory != view {
		t.Fatal("Memory was not the view passed to Open")
	}
	if got.Exec != execView {
		t.Fatal("Exec was not the view passed to WithExec")
	}
	if got.Files != nil {
		t.Fatal("WithExec set Files")
	}
}

type stubMemory struct{}

func (stubMemory) Snapshot() ctxgraph.Graph { return ctxgraph.Graph{} }

func (stubMemory) Commit(ctxgraph.Graph) error { return nil }

type stubFiles struct{}

func (stubFiles) Read(string) ([]byte, error)   { return nil, nil }
func (stubFiles) Write(string, []byte) error    { return nil }
func (stubFiles) Delete(string) error           { return nil }
func (stubFiles) Stat(string) (FileInfo, error) { return FileInfo{}, nil }
func (stubFiles) List(string) ([]DirEnt, error) { return nil, nil }

type stubExec struct{}

func (stubExec) Run(context.Context, Cmd) (ExecResult, error) {
	return ExecResult{}, nil
}
