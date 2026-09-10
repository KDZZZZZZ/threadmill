package manager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestManagerRunsIndependentTasksConcurrently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	releaseSecond := make(chan struct{})
	firstReported := make(chan struct{})
	created := false
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			switch {
			case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			case strings.Contains(request.SystemPrompt, "你是 manager"):
				if created {
					return agent.AssistantMessage{Content: "accepted"}, nil
				}
				created = true
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID: "create-independent", Name: "coordination_orchestrate",
					Arguments: json.RawMessage(`{"action":"replace_pending","tasks":[{"info":"first independent task"},{"info":"second independent task"}],"edges":[]}`),
				}}}, nil
			case strings.Contains(request.SystemPrompt, "你是 planner"):
				query := lastUser(request.Messages)
				started <- query
				select {
				case <-ctx.Done():
					return agent.AssistantMessage{}, ctx.Err()
				case <-release:
				}
				if strings.Contains(query, "first independent task") {
					return agent.AssistantMessage{}, errors.New("first task failed")
				}
				select {
				case <-ctx.Done():
					return agent.AssistantMessage{}, ctx.Err()
				case <-releaseSecond:
				}
			}
			return agent.AssistantMessage{Content: "role done"}, nil
		}),
		Output: func(text string) {
			if strings.Contains(text, "[任务报告]") && strings.Contains(text, "task-1") {
				close(firstReported)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		mgr.Close()
	}()
	mgr.Send("run both")
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent tasks did not both start before either completed")
		}
	}
	close(release)
	select {
	case <-firstReported:
	case <-ctx.Done():
		t.Fatal("failed task did not report while the independent task was running")
	}
	close(releaseSecond)
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	for _, task := range mgr.Snapshot().Tasks {
		want := coordination.OutcomeDone
		if task.ID == "task-1" {
			want = coordination.OutcomeFailed
		}
		if task.Outcome != want {
			t.Fatalf("%s outcome = %q, want %q", task.ID, task.Outcome, want)
		}
	}
}

func TestManagerPersistentTaskDoesNotBlockForegroundLifecycle(t *testing.T) {
	for _, action := range []string{"complete", "cancel"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			persistentStarted := make(chan context.Context, 1)
			persistentStopped := make(chan struct{})
			foregroundStarted := make(chan struct{})
			foregroundRelease := make(chan struct{})
			nextRequest := make(chan struct{})
			created := false
			mgr, err := Open(ctx, Options{
				Root: t.TempDir(),
				File: loadRepoConfig(t),
				Provider: stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
					query := lastUser(request.Messages)
					switch {
					case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
						return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
					case strings.Contains(request.SystemPrompt, "你是 manager"):
						if query == "next request" {
							close(nextRequest)
						}
						if created {
							return agent.AssistantMessage{Content: "accepted"}, nil
						}
						created = true
						return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
							ID: "create-tasks", Name: "coordination_orchestrate",
							Arguments: json.RawMessage(`{"action":"replace_pending","tasks":[{"id":"persistent","info":"persistent task","persistent":true},{"id":"foreground","info":"foreground task"}],"edges":[]}`),
						}}}, nil
					case strings.Contains(request.SystemPrompt, "你是 planner"):
						if strings.Contains(query, "persistent task") {
							persistentStarted <- callCtx
							<-callCtx.Done()
							close(persistentStopped)
							return agent.AssistantMessage{}, callCtx.Err()
						}
						close(foregroundStarted)
						select {
						case <-callCtx.Done():
							return agent.AssistantMessage{}, callCtx.Err()
						case <-foregroundRelease:
						}
					}
					return agent.AssistantMessage{Content: "role done"}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cancel()
				mgr.Close()
			}()
			mgr.Send("start")
			var persistentCtx context.Context
			select {
			case persistentCtx = <-persistentStarted:
			case <-ctx.Done():
				t.Fatal("persistent task did not start")
			}
			select {
			case <-foregroundStarted:
			case <-ctx.Done():
				t.Fatal("foreground task did not start")
			}
			wantOutcome := coordination.OutcomeDone
			if action == "cancel" {
				wantOutcome = coordination.OutcomeCanceled
				if !mgr.Cancel() {
					t.Fatal("Cancel did not cancel the foreground task")
				}
			} else {
				close(foregroundRelease)
			}
			if err := mgr.WaitIdle(ctx); err != nil {
				t.Fatal(err)
			}
			if err := persistentCtx.Err(); err != nil {
				t.Fatalf("foreground %s canceled persistent task: %v", action, err)
			}
			if mgr.Busy() {
				t.Fatal("independent persistent task keeps foreground busy")
			}
			for _, task := range mgr.Snapshot().Tasks {
				if task.ID == "foreground" && task.Outcome != wantOutcome {
					t.Fatalf("foreground outcome = %q, want %q", task.Outcome, wantOutcome)
				}
			}
			if got := mgr.Metrics().Tasks.Running; got != 1 {
				t.Fatalf("running tasks = %d, want persistent task only", got)
			}
			mgr.Send("next request")
			if err := mgr.WaitIdle(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case <-nextRequest:
			default:
				t.Fatal("persistent task blocked a later request")
			}
			mgr.Close()
			select {
			case <-persistentStopped:
			default:
				t.Fatal("Close returned before stopping persistent task execution")
			}
		})
	}
}
