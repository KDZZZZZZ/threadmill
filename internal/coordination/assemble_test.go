package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestAssembleInjectsTaskInfoIntoEveryRole(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	requests := make(map[string]agent.Request)
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		for _, role := range []string{"planner", "executor", "verifier"} {
			if strings.Contains(request.SystemPrompt, role+" role") {
				requests[role] = request
				return agent.AssistantMessage{Content: role + " done"}, nil
			}
		}
		return agent.AssistantMessage{}, fmt.Errorf("unknown role prompt %q", request.SystemPrompt)
	})

	stores := Stores{Memory: ctxgraph.NewStore()}
	roles, err := Assemble(
		stores,
		provider,
		agent.FileAgents{
			Planner: agent.FileAgent{
				SystemPrompt: "planner role",
				Hooks:        []string{"inject_subscribed_memory"},
			},
			Executor: agent.FileAgent{
				SystemPrompt: "executor role",
				Hooks:        []string{"inject_subscribed_memory"},
			},
			Verifier: agent.FileAgent{
				SystemPrompt: "verifier role",
				Hooks:        []string{"inject_subscribed_memory"},
			},
		},
		nil,
		0,
		nil,
	)(Task{ID: "task-1", Info: "literal acceptance oracle", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}

	for role, asker := range map[string]Asker{
		"planner":  roles.Planner,
		"executor": roles.Executor,
		"verifier": roles.Verifier,
	} {
		if _, err := asker.Ask(context.Background(), role+" input"); err != nil {
			t.Fatalf("%s Ask() error = %v", role, err)
		}
		want := "[Task Info] task-1: literal acceptance oracle"
		prompt := requests[role].WirePrompt()
		if !strings.Contains(prompt, want) {
			t.Errorf("%s wire prompt does not contain %q: %q", role, want, prompt)
		}
		if !requestBlockContains(requests[role].StableBlocks, want) {
			t.Errorf("%s task package is not in the stable prefix: %#v", role, requests[role].StableBlocks)
		}
		if requestBlockContains(requests[role].StateBlocks, want) {
			t.Errorf("%s task package remained in the dynamic tail: %#v", role, requests[role].StateBlocks)
		}
	}
}

func requestBlockContains(blocks []agent.Block, text string) bool {
	for _, block := range blocks {
		if strings.Contains(block.Text, text) {
			return true
		}
	}
	return false
}

func TestAssembleInjectsOriginalUserRequestIntoRootOnly(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	stores := Stores{Memory: ctxgraph.NewStore()}
	first := Task{ID: "task-1", Info: "first scoped contract", Env: Env{ID: "env-1"}}
	second := Task{ID: "task-2", Info: "second scoped contract", Env: Env{ID: "env-2", ParentID: "env-1"}}
	helper := Task{
		ID:          "task-3",
		Info:        "helper scoped contract",
		Env:         Env{ID: "env-3", ParentID: "env-1"},
		SpawnedFrom: first.ID,
	}
	if err := stores.ProjectManagerUserMessage("FIRST-REQUEST"); err != nil {
		t.Fatal(err)
	}
	if err := stores.ProjectManagerTaskInfos([]Task{first}); err != nil {
		t.Fatal(err)
	}
	if err := stores.ProjectManagerUserMessage("SECOND-REQUEST"); err != nil {
		t.Fatal(err)
	}
	if err := stores.ProjectManagerTaskInfos([]Task{first, second, helper}); err != nil {
		t.Fatal(err)
	}
	if err := stores.Memory.Fork(ManagerEnvID, first.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := stores.Memory.DropSubgraph(first.Env.ID, ManagerMemorySubgraphID); err != nil {
		t.Fatal(err)
	}
	if err := stores.Memory.Fork(first.Env.ID, second.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := stores.Memory.Fork(first.Env.ID, helper.Env.ID); err != nil {
		t.Fatal(err)
	}

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		return agent.AssistantMessage{Content: request.WirePrompt()}, nil
	})
	assemble := Assemble(
		stores,
		provider,
		agent.FileAgents{Planner: agent.FileAgent{
			SystemPrompt: "planner role",
			Hooks:        []string{"inject_subscribed_memory"},
		}},
		nil,
		0,
		nil,
	)

	tests := []struct {
		name     string
		task     Task
		want     []string
		unwanted []string
	}{
		{
			name:     "first root keeps its creation request",
			task:     first,
			want:     []string{"[User Message] FIRST-REQUEST", "[Task Info] task-1: first scoped contract"},
			unwanted: []string{"SECOND-REQUEST"},
		},
		{
			name:     "later root uses the later request",
			task:     second,
			want:     []string{"[User Message] SECOND-REQUEST", "[Task Info] task-2: second scoped contract"},
			unwanted: []string{"FIRST-REQUEST"},
		},
		{
			name:     "helper receives only its scoped contract",
			task:     helper,
			want:     []string{"[Task Info] task-3: helper scoped contract"},
			unwanted: []string{"FIRST-REQUEST", "SECOND-REQUEST"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			roles, err := assemble(test.task)
			if err != nil {
				t.Fatal(err)
			}
			prompt, err := roles.Planner.Ask(context.Background(), "plan")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt does not contain %q: %q", want, prompt)
				}
			}
			for _, unwanted := range test.unwanted {
				if strings.Contains(prompt, unwanted) {
					t.Errorf("prompt contains %q: %q", unwanted, prompt)
				}
			}
		})
	}
}

func TestAssembleDoesNotStartOrganizerForTaskPackage(t *testing.T) {
	stores := Stores{Memory: ctxgraph.NewStore()}
	roles, err := Assemble(
		stores,
		stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "done"}, nil
		}),
		agent.FileAgents{
			SubgraphOrganizer: agent.FileAgent{SystemPrompt: "organizer role"},
		},
		nil,
		0,
		nil,
	)(Task{ID: "task-1", Info: "self-contained task", Env: Env{ID: "env-1"}})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if roles.Prepare != nil {
		t.Fatal("Assemble() configured eager organizer work for a mechanically complete task package")
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

func TestGraphRunJoinToPlannerDoesNotAutomaticallyExposeCandidateFiles(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	root := graph.AddTask()
	target := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
	writer := mustSpawn(t, graph, root.Planner.ID, target.Planner.ID)
	coordTools := graph.HelpTools(nil)
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
				if strings.Contains(lastToolContent(request.Messages), "from writer") {
					return agent.AssistantMessage{}, errors.New("planner saw an unaccepted candidate file")
				}
				if strings.Contains(lastToolContent(request.Messages), `"discarded"`) {
					return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
						ID: "finish-join", Name: "join",
						Arguments: json.RawMessage(`{"action":"finish","session_id":"join:incoming:task-2:planner","reason":"candidate inspected"}`),
					}}}, nil
				}
				if !strings.Contains(lastToolContent(request.Messages), `"finished":true`) {
					return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
						ID: "discard-join", Name: "join",
						Arguments: json.RawMessage(`{"action":"discard","session_id":"join:incoming:task-2:planner","source_ids":["task-3"],"reason":"planning evidence only"}`),
					}}}, nil
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
		case strings.Contains(request.SystemPrompt, "verify role") && strings.Contains(input, "[join pending]"):
			if !hasToolResult(request.Messages) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID: "discard-child", Name: "join",
					Arguments: json.RawMessage(`{"action":"discard","session_id":"join:incoming:task-1:verifier","source_ids":["task-2"],"reason":"candidate is not part of this isolation test"}`),
				}}}, nil
			}
			if strings.Contains(lastToolContent(request.Messages), `"discarded"`) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID: "finish-child-join", Name: "join",
					Arguments: json.RawMessage(`{"action":"finish","session_id":"join:incoming:task-1:verifier","reason":"candidate disposition recorded"}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "verified"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role", Tools: []string{"join", "read"}},
		Executor: agent.FileAgent{SystemPrompt: "execute role", Tools: []string{"write"}},
		Verifier: agent.FileAgent{SystemPrompt: "verify role", Tools: []string{"join"}},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}

	if _, err := graph.Run(
		context.Background(),
		root.ID,
		"request",
		stores,
		Assemble(stores, provider, agents, nil, 0, nil, agent.FileOverlay{NamedTools: coordTools}),
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
	coordTools := graph.HelpTools(nil)
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
		case strings.Contains(input, "[join pending]"):
			if !hasToolResult(request.Messages) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "read-verification-fixture",
					Name:      "read",
					Arguments: json.RawMessage(`{"path":"joined.txt"}`),
				}}}, nil
			}
			if got := lastToolContent(request.Messages); strings.Contains(got, "fixture") {
				return agent.AssistantMessage{}, fmt.Errorf("verifier saw unaccepted joined fixture: %q", got)
			}
			if strings.Contains(lastToolContent(request.Messages), `"discarded"`) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID: "finish-join", Name: "join",
					Arguments: json.RawMessage(`{"action":"finish","session_id":"join:incoming:task-1:verifier","reason":"evidence inspected"}`),
				}}}, nil
			}
			if !strings.Contains(lastToolContent(request.Messages), `"finished":true`) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID: "discard-join", Name: "join",
					Arguments: json.RawMessage(`{"action":"discard","session_id":"join:incoming:task-1:verifier","source_ids":["task-2"],"reason":"fixture not adopted"}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "verified"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})
	agents := agent.FileAgents{
		Planner:  agent.FileAgent{SystemPrompt: "plan role"},
		Executor: agent.FileAgent{SystemPrompt: "execute role", Tools: []string{"write"}},
		Verifier: agent.FileAgent{SystemPrompt: "verify role", Tools: []string{"join", "read"}},
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}

	if _, err := graph.Run(
		context.Background(),
		root.ID,
		"request",
		stores,
		Assemble(stores, provider, agents, nil, 0, nil, agent.FileOverlay{NamedTools: coordTools}),
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
					Name:      coordOrchestrateName,
					Arguments: json.RawMessage(`{"action":"replace_pending","roots":[{"info":"do it"}]}`),
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
				Tools:        []string{coordOrchestrateName},
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
	if request.SystemPrompt != "yaml manager" {
		t.Fatalf("system prompt = %q, want the bare yaml manager prompt", request.SystemPrompt)
	}
	if !strings.Contains(requestBlock(request, "coordination"), "当前协调图（JSON：") {
		t.Fatalf("coordination block = %q, want injected graph", requestBlock(request, "coordination"))
	}
	if !hasTool(request.Tools, coordOrchestrateName) {
		t.Fatal("manager missing replacePending")
	}
	if graph.taskCount() != 1 {
		t.Fatalf("tasks = %d, want 1", graph.taskCount())
	}
	if !strings.Contains(requestBlock(after, "coordination"), `"ID":"task-1"`) {
		t.Fatalf("block after replacePending = %q, want latest task-1", requestBlock(after, "coordination"))
	}
}

func TestNewManagerLoopPublishesSelectedCompletedTask(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	graph := newGraph()
	task := graph.AddTask()
	base := t.TempDir()
	files := vfs.NewStore(base)
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Fork("", task.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.View(task.Env.ID).Write("selected.txt", []byte("published")); err != nil {
		t.Fatal(err)
	}
	if err := files.Archive(task.Env.ID, taskSnapshotEnvID(task)); err != nil {
		t.Fatal(err)
	}
	if err := graph.recordOutcome(task.ID, nil); err != nil {
		t.Fatal(err)
	}

	calls := 0
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		calls++
		if calls == 1 {
			if !hasTool(request.Tools, coordPublishTaskName) {
				t.Fatal("manager missing publishTask")
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "publish-1",
				Name:      coordPublishTaskName,
				Arguments: json.RawMessage(`{"task_id":"` + task.ID + `"}`),
			}}}, nil
		}
		return agent.AssistantMessage{Content: "delivered"}, nil
	})
	loop, err := NewManagerLoop(
		graph,
		stores,
		provider,
		agent.FileAgents{Manager: agent.FileAgent{
			SystemPrompt: "yaml manager",
			Tools:        []string{coordPublishTaskName},
		}},
		nil,
		0,
		agent.FileOverlay{},
	)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := loop.Ask(context.Background(), "publish the accepted task")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "delivered" {
		t.Fatalf("manager answer = %q", answer)
	}
	got, err := os.ReadFile(filepath.Join(base, "selected.txt"))
	if err != nil || string(got) != "published" {
		t.Fatalf("published file = %q, %v", got, err)
	}
}

func TestNewManagerLoopBindsOwnMemoryEnv(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	store := ctxgraph.NewStore()
	store.Save(ManagerEnvID, ctxgraph.Graph{
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
	if nodes := store.Load(ManagerEnvID).NodesInSubgraphs([]string{"bound"}); len(nodes) != 1 || nodes[0].ID != "n1" {
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
