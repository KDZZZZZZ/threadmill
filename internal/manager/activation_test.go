package manager

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestManagerStartsQueuedActivationWhileReviewingPreviousReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ends := make(chan event.RuntimeEvent, 2)
	reports := make(chan string, 2)
	releaseFirstEnd := make(chan struct{})
	releaseReports := make(chan struct{})
	var endCount atomic.Uint32
	provider := stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		if !strings.Contains(request.SystemPrompt, "你是 manager") {
			return agent.AssistantMessage{Content: "role complete"}, nil
		}
		query := lastUser(request.Messages)
		if strings.Contains(query, "[任务报告]") {
			reports <- query
			select {
			case <-releaseReports:
				return agent.AssistantMessage{Content: "report reviewed"}, nil
			case <-callCtx.Done():
				return agent.AssistantMessage{}, callCtx.Err()
			}
		}
		id := "start-service"
		args := `{"action":"replace_pending","tasks":[{"id":"service","info":"first activation","persistent":true}]}`
		if query == "continue service" {
			id = "continue-service"
			args = `{"action":"continue_task","task_id":"service","input":"second activation"}`
		}
		for _, message := range request.Messages {
			if result := message.ToolResult; result != nil && result.CallID == id {
				return agent.AssistantMessage{Content: "task scheduled"}, nil
			}
		}
		return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
			ID: id, Name: "coordination_orchestrate", Arguments: json.RawMessage(args),
		}}}, nil
	})
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(), File: loadRepoConfig(t), Provider: provider,
		OnEvent: func(eventCtx context.Context, item event.RuntimeEvent) {
			if item.Kind != event.KindTask || item.Phase != event.PhaseEnd {
				return
			}
			ends <- item
			if endCount.Add(1) == 1 {
				// The graph has finished activation 1, but Manager still owns its run.
				select {
				case <-releaseFirstEnd:
				case <-eventCtx.Done():
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	mgr.Send("start service")
	select {
	case ended := <-ends:
		if ended.Name != coordination.OutcomeIdle || ended.IsError {
			t.Fatalf("first activation ended as %+v", ended)
		}
	case <-ctx.Done():
		t.Fatal("first activation did not complete:", ctx.Err())
	}

	mgr.Send("continue service")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	task := mgr.Snapshot().Tasks[0]
	if task.Activation != 2 || task.Outcome != coordination.OutcomeActive {
		t.Fatalf("continuation was not queued: %+v", task)
	}
	close(releaseFirstEnd)
	select {
	case report := <-reports:
		if !strings.Contains(report, "目标: first activation") || !strings.Contains(report, "· idle ·") {
			t.Fatalf("old activation report was relabeled: %q", report)
		}
	case <-ctx.Done():
		t.Fatal("old activation report was lost:", ctx.Err())
	}

	// Reviewing the old report must not hold the explicitly continued activation.
	select {
	case ended := <-ends:
		if ended.Name != coordination.OutcomeIdle || ended.IsError {
			t.Fatalf("second activation inherited the old run's outcome or cancellation: %+v", ended)
		}
	case <-ctx.Done():
		t.Fatal("queued activation stalled while the previous report was under review")
	}
	close(releaseReports)
	select {
	case report := <-reports:
		if !strings.Contains(report, "目标: second activation") || !strings.Contains(report, "· idle ·") {
			t.Fatalf("new activation report was relabeled: %q", report)
		}
	case <-ctx.Done():
		t.Fatal("new activation report was lost:", ctx.Err())
	}
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}
