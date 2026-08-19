package coordination

import (
	"context"
	"encoding/json"
	"errors"
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
		if compactRequest(got) {
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

func TestAssembleUsesYamlToolDescription(t *testing.T) {
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
			Planner: agent.FileAgent{Tools: []string{"read"}},
		},
		nil,
		0,
		nil,
		agent.FileOverlay{
			Tools: agent.FileToolCatalog{
				"read": {Description: "yaml read intro"},
			},
		},
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}

	if _, err := roles.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	got := toolDescription(request.Tools, "read")
	if got != "yaml read intro" {
		t.Fatalf("read description = %q, want yaml read intro", got)
	}
}

func TestAssembleUsesYamlCompactPrompt(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	var compactPrompt string
	provider := stubProvider(func(_ context.Context, got agent.Request) (agent.AssistantMessage, error) {
		if got.SystemPrompt == "yaml compact" {
			compactPrompt = got.SystemPrompt
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
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
		agent.FileOverlay{
			Prompts: agent.FilePrompts{Compact: "yaml compact"},
		},
	)(Task{ID: "task-1", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}

	if _, err := roles.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if compactPrompt != "yaml compact" {
		t.Fatalf("compact prompt = %q, want yaml compact", compactPrompt)
	}
}

func TestNewManagerLoopReplacePendingMutatesGraph(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	stores := Stores{Memory: ctxgraph.NewStore()}
	var request, after agent.Request
	calls := 0
	provider := stubProvider(func(_ context.Context, got agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			request = got
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "r1",
					Name:      coordReplacePendingName,
					Arguments: json.RawMessage(`{"roots":[{"info":"do it"}]}`),
				}},
			}, nil
		}
		after = got
		return agent.AssistantMessage{Content: "done"}, nil
	})

	loop, err := NewManagerLoop(
		graph,
		stores,
		provider,
		agent.FileAgents{
			Manager: agent.FileAgent{
				SystemPrompt: "yaml manager",
				Tools:        []string{coordReplacePendingName},
			},
		},
		nil,
		0,
		agent.FileOverlay{},
	)
	if err != nil {
		t.Fatalf("NewManagerLoop() error = %v", err)
	}

	got, err := loop.Ask(context.Background(), "start")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if got != "done" {
		t.Fatalf("Ask() = %q, want done", got)
	}
	if !strings.HasPrefix(request.SystemPrompt, "yaml manager") {
		t.Fatalf("system prompt = %q, want prefix yaml manager", request.SystemPrompt)
	}
	if !strings.Contains(request.SystemPrompt, "当前协调图：") {
		t.Fatalf("system prompt = %q, want injected graph", request.SystemPrompt)
	}
	if !hasTool(request.Tools, coordReplacePendingName) {
		t.Fatal("manager missing replacePending")
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1", graph.taskCount())
	}
	if !strings.Contains(after.SystemPrompt, `"ID":"task-1"`) {
		t.Fatalf("prompt after replacePending = %q, want latest task-1", after.SystemPrompt)
	}
}

func TestNewManagerLoopBindsOwnMemoryEnv(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	store := ctxgraph.NewStore()
	store.Save(managerEnvID, ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "bound"}},
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "manager fact",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg-a"},
		}},
	})
	store.Save("env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "task fact",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg-a"},
		}},
	})

	calls := 0
	provider := stubProvider(func(_ context.Context, _ agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			return agent.AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "m1",
					Name:      "memory_add_to_subgraph",
					Arguments: json.RawMessage(`{"subgraph_id":"bound","node_ids":["n1"]}`),
				}},
			}, nil
		}
		return agent.AssistantMessage{Content: "done"}, nil
	})

	loop, err := NewManagerLoop(
		newGraph(),
		Stores{Memory: store},
		provider,
		agent.FileAgents{
			Manager: agent.FileAgent{Tools: []string{"memory_add_to_subgraph"}},
		},
		nil,
		0,
		agent.FileOverlay{},
	)
	if err != nil {
		t.Fatalf("NewManagerLoop() error = %v", err)
	}
	if _, err := loop.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if nodes := store.Load(managerEnvID).NodesInSubgraphs([]string{"bound"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("write did not stay in manager env")
	}
	if nodes := store.Load("env-1").NodesInSubgraphs([]string{"bound"}); len(nodes) != 0 {
		t.Fatal("write leaked to task env")
	}
}

func TestNewManagerLoopNilStore(t *testing.T) {
	_, err := NewManagerLoop(
		newGraph(),
		Stores{},
		stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "done"}, nil
		}),
		agent.FileAgents{},
		nil,
		0,
		agent.FileOverlay{},
	)
	if !errors.Is(err, ErrNilStore) {
		t.Fatalf("NewManagerLoop() error = %v, want %v", err, ErrNilStore)
	}
}

func compactRequest(got agent.Request) bool {
	for _, message := range got.Messages {
		if strings.Contains(message.Content, "待整理对话：") {
			return true
		}
	}
	return false
}

func toolDescription(tools []agenttool.Definition, name string) string {
	for _, tool := range tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	return ""
}
