package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestSessionRunsRootTaskAndWakesManagerWithReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var managerQueries []string
	var replies []string
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		switch {
		case strings.Contains(sys, "记忆整理器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "经理 Agent"):
			mu.Lock()
			query := lastUser(request.Messages)
			managerQueries = append(managerQueries, query)
			n := 0
			for _, q := range managerQueries {
				if !strings.Contains(q, "[任务报告]") {
					n++
				}
			}
			userTurns := n
			mu.Unlock()
			if strings.Contains(query, "[任务报告]") {
				return agent.AssistantMessage{Content: "任务已完成"}, nil
			}
			if userTurns == 1 && !hasToolResult(request.Messages) {
				args, err := json.Marshal(coordination.PendingSubgraph{
					Roots: []coordination.PendingRoot{{Info: "write hello"}},
				})
				if err != nil {
					return agent.AssistantMessage{}, err
				}
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "r1",
					Name:      "coordination.replacePending",
					Arguments: args,
				}}}, nil
			}
			return agent.AssistantMessage{Content: "已建任务"}, nil
		case strings.Contains(sys, "规划 Agent"):
			return agent.AssistantMessage{Content: "plan"}, nil
		case strings.Contains(sys, "执行 Agent"):
			return agent.AssistantMessage{Content: "did it"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})

	sess, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
		Output: func(text string) {
			mu.Lock()
			replies = append(replies, text)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer sess.Close()

	sess.Send("write hello")
	if err := sess.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}

	snap := sess.Snapshot()
	if len(snap.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(snap.Tasks))
	}
	if snap.Tasks[0].Outcome != coordination.OutcomeDone {
		t.Fatalf("outcome = %q, want done", snap.Tasks[0].Outcome)
	}
	if snap.Tasks[0].Info != "write hello" {
		t.Fatalf("info = %q, want write hello", snap.Tasks[0].Info)
	}

	mu.Lock()
	defer mu.Unlock()
	foundReport := false
	for _, q := range managerQueries {
		if strings.Contains(q, "[任务报告]") && strings.Contains(q, "task-1") && strings.Contains(q, "verified") {
			foundReport = true
		}
	}
	if !foundReport {
		t.Fatalf("manager queries = %q, want a task report", managerQueries)
	}
	joined := strings.Join(replies, "\n")
	if !strings.Contains(joined, "已建任务") || !strings.Contains(joined, "任务已完成") {
		t.Fatalf("replies = %q, want manager status then completion", replies)
	}
}

func TestSessionIdleWhenManagerOnlyTalks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if strings.Contains(request.SystemPrompt, "记忆整理器") {
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return agent.AssistantMessage{Content: "hello"}, nil
	})
	sess, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.Send("hi")
	if err := sess.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if n := len(sess.Snapshot().Tasks); n != 0 {
		t.Fatalf("tasks = %d, want 0", n)
	}
}

func loadRepoConfig(t *testing.T) provider.FileConfig {
	t.Helper()
	cfg, err := provider.LoadConfig(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type stubProvider func(context.Context, agent.Request) (agent.AssistantMessage, error)

func (f stubProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	return f(ctx, request)
}

func lastUser(messages []agent.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

func hasToolResult(messages []agent.Message) bool {
	for _, message := range messages {
		if message.Role == agent.RoleTool {
			return true
		}
	}
	return false
}

func TestFormatReport(t *testing.T) {
	t.Parallel()

	got := formatReport(coordination.Task{
		ID:      "task-3",
		Info:    "write the report",
		Outcome: coordination.OutcomeDone,
	}, "verified", nil, 4*time.Minute+12*time.Second, 12)
	want := "[任务报告] task-3 · done · 耗时 4m12s\n目标: write the report\ntoken: 12\nverifier 输出:\nverified"
	if got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}

	got = formatReport(coordination.Task{
		ID:      "task-1",
		Info:    "goal",
		Outcome: coordination.OutcomeFailed,
	}, "", errors.New("planner boom"), time.Second, 0)
	if !strings.Contains(got, "planner boom") || !strings.Contains(got, "failed") {
		t.Fatalf("failed report = %q", got)
	}
}
