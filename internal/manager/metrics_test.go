package manager

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
)

func TestManagerMetricsAndIdleSnapshotCoverRuntimeAndSubsystems(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var logs bytes.Buffer
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
				if sink := event.RetrySink(ctx); sink != nil {
					sink("transport")
				}
				return agent.AssistantMessage{
					Content: `{"nodes":[]}`,
					Usage: &agent.Usage{
						InputTokens: 4, CachedTokens: 3, CacheWriteTokens: 1, TotalTokens: 5,
					},
				}, nil
			}
			return agent.AssistantMessage{
				Content: "hello",
				Model:   "metrics-model",
				Usage: &agent.Usage{
					InputTokens: 6, CachedTokens: 4, CacheWriteTokens: 1, TotalTokens: 7,
				},
			}, nil
		}),
		Logger: logging.New(logging.Config{Output: &logs, JSON: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	mgr.Send("hi")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	got := mgr.Metrics()
	if got.Pending != 0 || got.TaskRunning || got.Events.Model.Completed != 1 || got.Events.Tokens != 7 {
		t.Fatalf("metrics = %#v", got)
	}
	if got.Events.MemoryTokens != 5 || got.Events.MemoryRetries != 1 {
		t.Fatalf("memory model cost = %#v", got.Events)
	}
	if got.Runtime.Goroutines <= 0 || got.Runtime.HeapAlloc == 0 {
		t.Fatalf("runtime metrics = %#v", got.Runtime)
	}
	if got.Exec.Capacity <= 0 || got.VFS.LiveDirs != 0 || got.Memory.Environments == 0 {
		t.Fatalf("subsystem metrics = %#v", got)
	}
	for _, field := range []string{
		`"msg":"runtime snapshot"`,
		`"model_active":0`,
		`"model_p50":`,
		`"model_max":`,
		`"model_ttft_max":`,
		`"model_delta_chunks":`,
		`"model_delta_bytes":`,
		`"model_stream_chunks":`,
		`"model_stream_idle":`,
		`"input_tokens":6`,
		`"cached_tokens":4`,
		`"cache_write_tokens":1`,
		`"cache_hit_rate":`,
		`"total_cache_hit_rate":`,
		`"tool_max":`,
		`"tool_active":0`,
		`"task_max":`,
		`"memory_ops_max":`,
		`"memory_ops_active":0`,
		`"memory_ops_tokens":5`,
		`"memory_input_tokens":4`,
		`"memory_cached_tokens":3`,
		`"memory_cache_write_tokens":1`,
		`"memory_ops_retries":1`,
		`"memory_stream_chunks":`,
		`"memory_stream_idle":`,
		`"memory_ttft_max":`,
		`"memory_organizer_tokens":`,
		`"memory_organizer_p95":`,
		`"total_tokens":12`,
		`"tasks_total":0`,
		`"exec_capacity":`,
		`"exec_sandbox_backend":`,
		`"exec_network_isolation":`,
		`"exec_peak_queued":0`,
		`"exec_wait_duration":`,
		`"exec_run_duration":`,
		`"exec_canceled":0`,
		`"exec_timed_out":0`,
		`"exec_tracked_process_groups":0`,
		`"exec_runtime_dirs":0`,
		`"vfs_overlay_files":0`,
		`"vfs_tombstones":0`,
		`"vfs_materialize_copies":`,
		`"vfs_materialize_copy_errors":`,
		`"vfs_materialize_copy_duration":`,
		`"vfs_handoffs":`,
		`"memory_baselines":0`,
		`"memory_subgraphs":`,
		`"heap_objects":`,
		`"gc_pause_total":`,
	} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("logs = %s, want field %s", logs.String(), field)
		}
	}
}

func TestManagerLogsPeriodicSnapshotWhileBusy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logs bytes.Buffer
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "ok"}, nil
		}),
		Logger: logging.New(logging.Config{Output: &logs, JSON: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	mgr.mu.Lock()
	mgr.pending = 1
	mgr.mu.Unlock()
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	close(ticks)
	mgr.monitorSnapshots(context.Background(), ticks)
	if !strings.Contains(logs.String(), `"msg":"runtime snapshot"`) {
		t.Fatalf("logs = %s, want periodic snapshot", logs.String())
	}

	mgr.mu.Lock()
	mgr.pending = 0
	mgr.mu.Unlock()
	before := logs.Len()
	ticks = make(chan time.Time, 1)
	ticks <- time.Now()
	close(ticks)
	mgr.monitorSnapshots(context.Background(), ticks)
	if logs.Len() != before {
		t.Fatalf("idle tick wrote %d bytes", logs.Len()-before)
	}
}

func TestManagerBurst100MessagesDrainsWithObservableCounts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			}
			return agent.AssistantMessage{
				Content: "ok",
				Model:   "burst-model",
				Usage:   &agent.Usage{TotalTokens: 1},
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	for i := range 100 {
		mgr.Send("burst " + string(rune('a'+i%26)))
	}
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	got := mgr.Metrics()
	if got.Pending != 0 || got.TaskRunning || got.Events.Model.Completed != 100 || got.Events.Model.Errors != 0 {
		t.Fatalf("metrics = %#v", got)
	}
	if got.Events.Tool.Completed != 0 || got.Events.Memory.Completed != 200 || got.Events.Memory.Errors != 0 {
		t.Fatalf("hidden memory metrics = %#v", got.Events.Memory)
	}
}

func TestManagerLogsSnapshotWhenProviderFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var logs bytes.Buffer
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{}, errors.New("provider unavailable")
		}),
		Logger: logging.New(logging.Config{Output: &logs, JSON: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	mgr.Send("fail")
	if err := mgr.WaitIdle(ctx); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if mgr.Busy() {
		t.Fatal("manager is still settling after WaitIdle")
	}
	if !strings.Contains(logs.String(), `"msg":"runtime snapshot"`) ||
		!strings.Contains(logs.String(), `"model_errors":1`) {
		t.Fatalf("logs = %s, want error snapshot", logs.String())
	}
}
