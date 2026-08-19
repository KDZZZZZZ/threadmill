package coordination

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestAssembleUsesYamlToolsHooksAndPrompt(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	var request agent.Request
	provider := stubProvider(func(_ context.Context, got agent.Request) (agent.AssistantMessage, error) {
		request = got
		return agent.AssistantMessage{Content: "done"}, nil
	})

	roles, err := Assemble(
		Stores{Memory: ctxgraph.NewStore()},
		provider,
		agent.FileAgents{
			Planner: agent.FileAgent{
				SystemPrompt: "yaml plan",
				Tools:        []string{"organize_subgraph"},
				Hooks:        []string{"inject_subscribed_memory"},
			},
		},
		nil,
		0,
		nil,
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}

	got, err := roles.Planner.Ask(context.Background(), "start")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if got != "done" {
		t.Fatalf("Ask() = %q, want done", got)
	}
	if request.SystemPrompt != "yaml plan" {
		t.Fatalf("planner prompt = %q, want yaml plan", request.SystemPrompt)
	}
	if !hasTool(request.Tools, "organize_subgraph") {
		t.Fatal("yaml organize_subgraph missing")
	}
	if hasTool(request.Tools, "memory_add_to_subgraph") {
		t.Fatal("planner gained memory tools not listed in yaml")
	}
	if hasTool(request.Tools, "inject_subscribed_memory") || hasTool(request.Tools, "compact_memory") {
		t.Fatal("hidden memory tools leaked to the model")
	}
}

func TestAssembleBindsVFSFiles(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	files := vfs.NewStore(t.TempDir())
	calls := 0
	provider := stubProvider(func(_ context.Context, _ agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "w1",
					Name:      "write",
					Arguments: json.RawMessage(`{"path":"a.txt","content":"from-assemble"}`),
				}},
			}, nil
		}
		return agent.AssistantMessage{Content: "done"}, nil
	})

	roles, err := Assemble(
		Stores{Memory: ctxgraph.NewStore(), Files: files},
		provider,
		agent.FileAgents{
			Executor: agent.FileAgent{Tools: []string{"write"}},
		},
		nil,
		0,
		nil,
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if _, err := roles.Executor.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	got, err := files.View("env-1").Read("a.txt")
	if err != nil {
		t.Fatalf("Read after write: %v", err)
	}
	if string(got) != "from-assemble" {
		t.Fatalf("a.txt = %q, want from-assemble", got)
	}
}

func TestAssembleBindsExec(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	files := vfs.NewStore(t.TempDir())
	sched := tmexec.New(tmexec.Config{Slots: 1})
	calls := 0
	provider := stubProvider(func(_ context.Context, _ agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "b1",
					Name:      "bash",
					Arguments: json.RawMessage(`{"command":"true"}`),
				}},
			}, nil
		}
		return agent.AssistantMessage{Content: "done"}, nil
	})

	roles, err := Assemble(
		Stores{Memory: ctxgraph.NewStore(), Files: files, Exec: sched},
		provider,
		agent.FileAgents{
			Executor: agent.FileAgent{Tools: []string{"bash"}},
		},
		nil,
		0,
		nil,
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	got, err := roles.Executor.Ask(context.Background(), "start")
	if err != nil {
		if strings.Contains(err.Error(), "not bound to env") {
			t.Fatalf("bash stayed unbound: %v", err)
		}
		if !strings.Contains(err.Error(), "SANDBOX_UNAVAILABLE") {
			t.Fatalf("Ask() error = %v", err)
		}
		return
	}
	if got != "done" {
		t.Fatalf("Ask() = %q, want done", got)
	}
}

func TestAssembleUsesLLMContextWindowForOverflowCompact(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	var sawOrganize bool
	var request agent.Request
	provider := stubProvider(func(_ context.Context, got agent.Request) (agent.AssistantMessage, error) {
		if got.SystemPrompt == agent.OrganizePrompt {
			sawOrganize = true
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		request = got
		return agent.AssistantMessage{
			Content: "done " + strings.Repeat("tail ", 20),
			Usage:   &agent.Usage{TotalTokens: 50},
		}, nil
	})

	roles, err := Assemble(
		Stores{Memory: ctxgraph.NewStore()},
		provider,
		agent.FileAgents{
			Planner: agent.FileAgent{
				Hooks: []string{"compact_on_overflow"},
			},
		},
		nil,
		5,
		nil,
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	roles.Planner.(*agent.Loop).SetSubscribedSubgraphs([]string{"sg-a"})

	if _, err := roles.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if !sawOrganize {
		t.Fatal("compact_on_overflow did not run, want llm.context_window applied")
	}
	if hasTool(request.Tools, "compact_memory") {
		t.Fatal("compact_memory leaked to the model")
	}
}
