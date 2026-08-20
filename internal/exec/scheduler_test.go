package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestSchedulerRespectsSlotLimit(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	var current, max atomic.Int32
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		n := current.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		current.Add(-1)
		return env.ExecResult{}, nil
	}

	files := vfs.NewStore(t.TempDir())
	view := s.View("env-a", files)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	wg.Wait()
	if max.Load() > 1 {
		t.Fatalf("concurrent runs = %d, want at most 1", max.Load())
	}
}

func TestSchedulerStatsExposeQueueSaturationAndCompletion(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()
	<-entered
	go func() {
		defer wg.Done()
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		stats := s.Stats()
		if stats.Active == 1 && stats.Queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats before release = %#v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	got := s.Stats()
	if got.Capacity != 1 || got.Requests != 2 || got.Started != 2 || got.Completed != 2 {
		t.Fatalf("stats = %#v", got)
	}
	if got.Active != 0 || got.Queued != 0 || got.PeakActive != 1 || got.PeakQueued < 1 {
		t.Fatalf("saturation stats = %#v", got)
	}
	if got.WaitDuration <= 0 || got.RunDuration <= 0 {
		t.Fatalf("durations = %#v", got)
	}
}

func TestSchedulerStatsClassifyCanceledWait(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		close(entered)
		<-release
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.Run(ctx, env.Cmd{Command: "true"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	close(release)
	<-done
	got := s.Stats()
	if got.Requests != 2 || got.Completed != 2 || got.Canceled != 1 || got.Errors != 1 {
		t.Fatalf("stats = %#v", got)
	}
}

func TestSchedulerStatsDoNotCountImmediateSlotAsQueued(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	got := s.Stats()
	if got.Queued != 0 || got.PeakQueued != 0 || got.WaitDuration != 0 {
		t.Fatalf("queue stats = %#v", got)
	}
}

func TestSchedulerLeavesLiveWritesForRelease(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	var live string
	s.run = func(_ context.Context, dir string, _ env.Cmd) (env.ExecResult, error) {
		live = dir
		if err := os.WriteFile(filepath.Join(dir, "from-run.txt"), []byte("x"), 0o640); err != nil {
			return env.ExecResult{}, err
		}
		return env.ExecResult{}, nil
	}
	files := vfs.NewStore(t.TempDir())
	if err := files.View("env-a").Write("overlay.txt", []byte("overlay")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live dir after Run: %v", err)
	}
	if err := files.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := files.View("env-a").Read("from-run.txt")
	if err != nil {
		t.Fatalf("Release dropped live write: %v", err)
	}
	if string(got) != "x" {
		t.Fatalf("from-run.txt = %q, want x", got)
	}
}

func TestSchedulerUnavailableSandbox(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	s.sandbox = sandboxNone
	s.run = nil
	files := vfs.NewStore(t.TempDir())
	_, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"})
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Run error = %v, want ErrSandboxUnavailable", err)
	}
	if err != nil && !strings.Contains(err.Error(), "SANDBOX_UNAVAILABLE") {
		t.Fatalf("error %v missing SANDBOX_UNAVAILABLE", err)
	}
}

func TestSchedulerReapKillsTrackedProcessGroup(t *testing.T) {
	t.Parallel()

	cmd := osexec.Command("bash", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	s := New(Config{Slots: 1})
	s.track("env-a", pgid)
	s.Reap("env-a")
	if err := cmd.Wait(); err == nil {
		t.Fatal("sleep still running after Reap")
	}
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("process group still alive after Reap")
	}
}

func TestSchedulerWithoutBwrapDoesNotRun(t *testing.T) {
	t.Parallel()
	if probeBwrap() {
		t.Skip("bwrap available")
	}

	s := New(Config{Slots: 1})
	files := vfs.NewStore(t.TempDir())
	_, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"})
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Run error = %v, want ErrSandboxUnavailable", err)
	}
}
