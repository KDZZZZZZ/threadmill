package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestTraceExportIsDeterministicAndComplete(t *testing.T) {
	cfg := traceGeneration{Agents: 2, Turns: 3, Files: 4, Seed: 42, Think: 0, Scale: 0.01}
	a, err := generateTrace(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateTrace(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(a)
	second, _ := json.Marshal(b)
	if !bytes.Equal(first, second) {
		t.Fatal("same seed produced different traces")
	}
	if len(a.Agents) != 2 || a.Agents[0].ID != "agent-0" {
		t.Fatalf("agent identities = %#v", a.Agents)
	}
	for _, agent := range a.Agents {
		thinks := 0
		for _, op := range agent.Operations {
			if op.Op == "think" {
				thinks++
			}
		}
		if thinks != 3 {
			t.Fatalf("%s has %d thoughts, want 3", agent.ID, thinks)
		}
	}
	if _, err := decodeTrace(bytes.NewReader(first)); err != nil {
		t.Fatalf("exported trace cannot be replayed: %v", err)
	}
}

func TestTraceReplayCollectsAndReleasesEvenForExpectedFailure(t *testing.T) {
	root := t.TempDir()
	if !vfs.ReflinkCloneable(root, root) {
		t.Skip("replay CLI needs a reflink filesystem; set TMPDIR to a Btrfs test directory")
	}
	repo, err := makeFixtureRepo(root, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitFixture(repo); err != nil {
		t.Fatal(err)
	}
	commit, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	trace := loadTrace{Version: 1, Fixture: traceFixture{Kind: "repository", Commit: commit},
		Agents: []traceAgent{{ID: "worker", Operations: []traceOp{
			{Op: "bash", Command: "printf 'saved result\\n' > result.txt; exit 7", ExpectedExit: 7},
			{Op: "read", Path: "result.txt"},
		}}}}
	cfg := replayOptions{Repo: repo, Workdir: filepath.Join(root, "runtime"), Sandbox: "external", Slots: 1, Retain: true}
	report, err := runTrace(context.Background(), trace, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 || report.Latency["fork"].Count != 1 ||
		report.Latency["collect"].Count != 1 || report.Latency["release"].Count != 1 {
		t.Fatalf("incomplete lifecycle report: %+v", report)
	}
	if report.Execution.RuntimeDirs != 0 {
		t.Fatalf("runtime directories retained after release: %d", report.Execution.RuntimeDirs)
	}
	if _, err := runTrace(context.Background(), trace, cfg); err == nil || !strings.Contains(err.Error(), "empty workdir") {
		t.Fatalf("replay into retained state = %v, want an empty workdir requirement", err)
	}
	store, err := vfs.NewPersistentStore(filepath.Join(cfg.Workdir, "source"), filepath.Join(cfg.Workdir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Restore("checkpoint-worker"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("checkpoint-worker").Read("result.txt")
	if err != nil || string(got) != "saved result\n" {
		t.Fatalf("collected command writes = %q, %v", got, err)
	}
	if err := store.Restore("worker"); !errors.Is(err, vfs.ErrUnknownEnvironment) {
		t.Fatalf("released worker restore = %v, want unknown environment", err)
	}
}

func TestTraceReplayRejectsAnUnsafePath(t *testing.T) {
	raw := `{"version":1,"seed":42,"fixture":{"kind":"synthetic","files":1,"file_bytes":4096},"agents":[{"id":"agent-0","operations":[{"op":"write","path":"../other-agent/file","content":"changed"}]}]}`
	if _, err := decodeTrace(strings.NewReader(raw)); err == nil {
		t.Fatal("replay accepted a path outside the workspace")
	}
}
