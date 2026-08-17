package coordination

import (
	"context"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
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
		ctxgraph.NewStore(),
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
}

func TestAssembleUsesLLMContextWindowForOverflowCompact(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	var sawOrganize bool
	provider := stubProvider(func(_ context.Context, got agent.Request) (agent.AssistantMessage, error) {
		if got.SystemPrompt == agent.OrganizePrompt {
			sawOrganize = true
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return agent.AssistantMessage{
			Content: "done " + strings.Repeat("tail ", 20),
			Usage:   &agent.Usage{TotalTokens: 50},
		}, nil
	})

	roles, err := Assemble(
		ctxgraph.NewStore(),
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
}
