package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func TestGraphRunKeepsExecutorFilesButDiscardsPlannerAndVerifierFiles(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	files := vfs.NewStore(t.TempDir())
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		var role, path, content string
		switch {
		case strings.Contains(request.SystemPrompt, "plan role"):
			role, path, content = "planner", "planner.txt", "scratch"
		case strings.Contains(request.SystemPrompt, "execute role"):
			if got := firstUserContent(request.Messages); got != "the plan" {
				return agent.AssistantMessage{}, errors.New("executor did not receive the planner artifact")
			}
			role, path, content = "executor", "executor.txt", "kept"
		case strings.Contains(request.SystemPrompt, "verify role"):
			role, path, content = "verifier", "verifier.txt", "scratch"
		default:
			return agent.AssistantMessage{}, errors.New("unknown role")
		}
		if !hasToolResult(request.Messages) {
			args, err := json.Marshal(map[string]string{"path": path, "content": content})
			if err != nil {
				return agent.AssistantMessage{}, err
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        role + "-write",
				Name:      "write",
				Arguments: args,
			}}}, nil
		}
		switch role {
		case "planner":
			return agent.AssistantMessage{Content: "the plan"}, nil
		case "executor":
			return agent.AssistantMessage{Content: "executed"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role", Tools: []string{"write"}},
		Executor: agent.FileAgent{SystemPrompt: "execute role", Tools: []string{"write"}},
		Verifier: agent.FileAgent{SystemPrompt: "verify role", Tools: []string{"write"}},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	graph := newGraph()
	task := graph.AddTask()

	got, err := graph.Run(
		context.Background(),
		task.ID,
		"request",
		stores,
		Assemble(stores, provider, agents, nil, 0, nil),
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got != "verified" {
		t.Fatalf("Run() = %q, want verified", got)
	}
	if body, err := files.View(task.Env.ID).Read("executor.txt"); err != nil || string(body) != "kept" {
		t.Fatalf("executor.txt = %q, %v; want kept", body, err)
	}
	for _, path := range []string{"planner.txt", "verifier.txt"} {
		if _, err := files.View(task.Env.ID).Read(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived its disposable role workspace: %v", path, err)
		}
	}
}

func TestGraphRunJoinToPlannerUsesDisposableWorkspace(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	root := graph.AddTask()
	target := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
	writer := mustSpawn(t, graph, root.Planner.ID, target.Planner.ID)
	setTaskInfo(t, graph, target.ID, "target task")
	setTaskInfo(t, graph, writer.ID, "writer task")
	files := vfs.NewStore(t.TempDir())
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		input := firstUserContent(request.Messages)
		switch {
		case strings.Contains(request.SystemPrompt, "plan role"):
			switch {
			case strings.Contains(input, "writer task"):
				return agent.AssistantMessage{Content: "writer plan"}, nil
			case strings.Contains(input, "target task"):
				if !hasToolResult(request.Messages) {
					return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
						ID:        "read-joined-code",
						Name:      "read",
						Arguments: json.RawMessage(`{"path":"joined.txt"}`),
					}}}, nil
				}
				if !strings.Contains(lastToolContent(request.Messages), "from writer") {
					return agent.AssistantMessage{}, errors.New("planner did not see joined code")
				}
				return agent.AssistantMessage{Content: "target plan"}, nil
			default:
				return agent.AssistantMessage{Content: "root plan"}, nil
			}
		case strings.Contains(request.SystemPrompt, "execute role"):
			if input == "writer plan" && !hasToolResult(request.Messages) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "write-joined-code",
					Name:      "write",
					Arguments: json.RawMessage(`{"path":"joined.txt","content":"from writer"}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "executed"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role", Tools: []string{"read"}},
		Executor: agent.FileAgent{SystemPrompt: "execute role", Tools: []string{"write"}},
		Verifier: agent.FileAgent{SystemPrompt: "verify role"},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}

	if _, err := graph.Run(
		context.Background(),
		root.ID,
		"request",
		stores,
		Assemble(stores, provider, agents, nil, 0, nil),
	); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := files.View(root.Env.ID).Read("joined.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planner join leaked child implementation: %v", err)
	}
}

func TestGraphRunJoinToVerifierUsesDisposableWorkspace(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	root := graph.AddTask()
	helper := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	setTaskInfo(t, graph, helper.ID, "verification helper")
	files := vfs.NewStore(t.TempDir())
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		input := firstUserContent(request.Messages)
		switch {
		case strings.Contains(request.SystemPrompt, "plan role"):
			if strings.Contains(input, "verification helper") {
				return agent.AssistantMessage{Content: "helper plan"}, nil
			}
			return agent.AssistantMessage{Content: "plan"}, nil
		case strings.Contains(request.SystemPrompt, "execute role"):
			if input != "helper plan" || hasToolResult(request.Messages) {
				return agent.AssistantMessage{Content: "executed"}, nil
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "write-verification-fixture",
				Name:      "write",
				Arguments: json.RawMessage(`{"path":"joined.txt","content":"fixture"}`),
			}}}, nil
		case strings.Contains(input, "[join]"):
			if !hasToolResult(request.Messages) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "read-verification-fixture",
					Name:      "read",
					Arguments: json.RawMessage(`{"path":"joined.txt"}`),
				}}}, nil
			}
			if got := lastToolContent(request.Messages); !strings.Contains(got, "fixture") {
				return agent.AssistantMessage{}, fmt.Errorf("verifier did not see joined fixture: %q", got)
			}
			return agent.AssistantMessage{Content: "verified"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role"},
		Executor: agent.FileAgent{SystemPrompt: "execute role", Tools: []string{"write"}},
		Verifier: agent.FileAgent{SystemPrompt: "verify role", Tools: []string{"read"}},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}

	if _, err := graph.Run(
		context.Background(),
		root.ID,
		"request",
		stores,
		Assemble(stores, provider, agents, nil, 0, nil),
	); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if _, err := files.View(root.Env.ID).Read("joined.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verifier join leaked helper implementation: %v", err)
	}
}

func TestGraphRunKeepsPlannerWorkspaceAfterFailureAndDiscardsItAfterResume(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	files := vfs.NewStore(t.TempDir())
	crashed := errors.New("planner crashed")
	plannerCalls := 0
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		switch {
		case strings.Contains(request.SystemPrompt, "plan role"):
			plannerCalls++
			switch plannerCalls {
			case 1:
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "write-before-crash",
					Name:      "write",
					Arguments: json.RawMessage(`{"path":"resume.txt","content":"recover me"}`),
				}}}, nil
			case 2:
				return agent.AssistantMessage{}, crashed
			case 3:
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "read-after-crash",
					Name:      "read",
					Arguments: json.RawMessage(`{"path":"resume.txt"}`),
				}}}, nil
			default:
				if !strings.Contains(lastToolContent(request.Messages), "recover me") {
					return agent.AssistantMessage{}, errors.New("planner scratch was not recovered")
				}
				return agent.AssistantMessage{Content: "recovered plan"}, nil
			}
		case strings.Contains(request.SystemPrompt, "execute role"):
			return agent.AssistantMessage{Content: "executed"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role", Tools: []string{"read", "write"}},
		Executor: agent.FileAgent{SystemPrompt: "execute role"},
		Verifier: agent.FileAgent{SystemPrompt: "verify role"},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	graph := newGraph()
	task := graph.AddTask()
	assemble := Assemble(stores, provider, agents, nil, 0, nil)

	if _, err := graph.Run(context.Background(), task.ID, "request", stores, assemble); !errors.Is(err, crashed) {
		t.Fatalf("first Run() error = %v, want %v", err, crashed)
	}
	if _, err := graph.Run(context.Background(), task.ID, "request", stores, assemble); err != nil {
		t.Fatalf("resume Run() error = %v", err)
	}
	if _, err := files.View(task.Env.ID).Read("resume.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planner scratch leaked into task env after resume: %v", err)
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
