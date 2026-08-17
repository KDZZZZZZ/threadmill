package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestSchedulerDoesNotAbsorbOnRun(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	s.run = func(_ context.Context, live string, _ env.Cmd) (env.ExecResult, error) {
		if err := os.WriteFile(filepath.Join(live, "from-run.txt"), []byte("x"), 0o640); err != nil {
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
	if _, err := files.View("env-a").Read("from-run.txt"); err == nil {
		t.Fatal("Run absorbed a live file into overlay")
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
