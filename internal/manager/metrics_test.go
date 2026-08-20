package manager

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
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
		Provider: stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "记忆整理器") {
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			}
			return agent.AssistantMessage{
				Content: "hello",
				Model:   "metrics-model",
				Usage:   &agent.Usage{TotalTokens: 7},
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
	if got.Runtime.Goroutines <= 0 || got.Runtime.HeapAlloc == 0 {
		t.Fatalf("runtime metrics = %#v", got.Runtime)
	}
	if got.Exec.Capacity <= 0 || got.VFS.LiveDirs != 0 || got.Memory.Environments == 0 {
		t.Fatalf("subsystem metrics = %#v", got)
	}
	if !strings.Contains(logs.String(), `"msg":"runtime snapshot"`) {
		t.Fatalf("logs = %s, want runtime snapshot", logs.String())
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
			if strings.Contains(request.SystemPrompt, "记忆整理器") {
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
