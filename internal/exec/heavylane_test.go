package exec

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

type laneRunner struct {
	calls    atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
	release  chan struct{}
}

func (r *laneRunner) run(ctx context.Context, live string, spec env.Cmd) (env.ExecResult, error) {
	current := r.inFlight.Add(1)
	for {
		old := r.peak.Load()
		if current <= old || r.peak.CompareAndSwap(old, current) {
			break
		}
	}
	defer r.inFlight.Add(-1)
	r.calls.Add(1)
	if r.release != nil {
		<-r.release
	} else {
		time.Sleep(60 * time.Millisecond)
	}
	return env.ExecResult{ExitCode: 0}, nil
}

func newLaneScheduler(t *testing.T, runner *laneRunner, cfg Config) (*Scheduler, *vfs.Store) {
	t.Helper()
	store, err := vfs.NewPersistentStore(t.TempDir(), filepath.Join(t.TempDir(), "live"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExternalSandbox = true
	s := New(cfg)
	s.run = runner.run
	return s, store
}

func TestHeavyLaneSerializesHeavyCommands(t *testing.T) {
	runner := &laneRunner{}
	s, store := newLaneScheduler(t, runner, Config{Slots: 8, Timeout: 5 * time.Second})
	// 预置成本画像：cargo test 历史均值 15s → 重车道。
	s.costs.record("cargo test", 15*time.Second, 0)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("env-heavy", store).Run(context.Background(), env.Cmd{Command: "cargo test"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := runner.peak.Load(); got != 1 {
		t.Errorf("peak concurrent heavy runs = %d, want 1 (heavy lane serializes)", got)
	}
}

func TestHeavyLaneProtectsColdBuildCommands(t *testing.T) {
	runner := &laneRunner{}
	s, store := newLaneScheduler(t, runner, Config{
		Slots: 8, HeavySlots: 1, Timeout: 5 * time.Second,
	})

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("env-cold-build", store).Run(
				context.Background(), env.Cmd{Command: "go test ./..."},
			); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := runner.peak.Load(); got != 1 {
		t.Errorf("peak concurrent cold builds = %d, want 1", got)
	}
}

func TestLightCommandsShareGeneralPool(t *testing.T) {
	runner := &laneRunner{}
	s, store := newLaneScheduler(t, runner, Config{Slots: 4, Timeout: 5 * time.Second})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("env-light", store).Run(context.Background(), env.Cmd{Command: "ls ."}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := runner.peak.Load(); got != 3 {
		t.Errorf("peak concurrent light runs = %d, want 3 (no heavy lane for unknown commands)", got)
	}
}

func TestMemoryBudgetThrottlesKnownHeavyCommands(t *testing.T) {
	runner := &laneRunner{}
	s, store := newLaneScheduler(t, runner, Config{
		Slots: 8, Timeout: 5 * time.Second,
		MemoryBudgetBytes: 1024 * 1024,
	})
	// 预置峰值画像：go test ./... 峰值 800KB，预算 1MB → 同时只能跑一条。
	s.costs.record("go test ./...", time.Second, 800*1024)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("env-mem", store).Run(context.Background(), env.Cmd{Command: "go test ./..."}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := runner.peak.Load(); got != 1 {
		t.Errorf("peak concurrent memory-heavy runs = %d, want 1 (budget admission)", got)
	}
}

func TestHeavyLaneStatsExposeSaturation(t *testing.T) {
	runner := &laneRunner{release: make(chan struct{})}
	s, store := newLaneScheduler(t, runner, Config{
		Slots: 8, HeavySlots: 1, Timeout: 5 * time.Second,
	})
	s.costs.record("cargo test", 15*time.Second, 0)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("env-heavy-stats", store).Run(
				context.Background(), env.Cmd{Command: "cargo test"},
			); err != nil {
				t.Error(err)
			}
		}()
	}

	deadline := time.Now().Add(time.Second)
	for {
		stats := s.Stats()
		if stats.HeavyActive == 1 && stats.HeavyQueued == 1 {
			if stats.HeavyCapacity != 1 || stats.HeavyPeakActive != 1 {
				t.Fatalf("heavy stats = %#v", stats)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heavy lane did not saturate: %#v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	close(runner.release)
	wg.Wait()

	stats := s.Stats()
	if stats.HeavyActive != 0 || stats.HeavyQueued != 0 || stats.HeavyWaitDuration <= 0 {
		t.Fatalf("heavy stats after completion = %#v", stats)
	}
}
