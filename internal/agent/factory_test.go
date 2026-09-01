package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestNewSubgraphOrganizerRegistersMemoryTools(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	organizer, err := NewSubgraphOrganizer(Config{
		Provider: ignoreOrganize(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		SystemPrompt: "yaml organize",
	})
	if err != nil {
		t.Fatal(err)
	}

	answer, err := organizer.Ask(context.Background(), "blue")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if request.SystemPrompt != "yaml organize" {
		t.Fatalf("system prompt = %q, want yaml organize", request.SystemPrompt)
	}

	got := make(map[string]struct{}, len(request.Tools))
	for _, def := range request.Tools {
		got[def.Name] = struct{}{}
	}
	for _, name := range []string{
		"memory_neighbors",
		"memory_subgraphs_of",
		"memory_sources_of",
		"memory_nodes_in",
		"memory_add_to_subgraph",
	} {
		if _, ok := got[name]; !ok {
			t.Fatalf("request tools missing %q: %#v", name, request.Tools)
		}
	}
}

func TestMemoryToolsUseAgentCopyNotGlobal(t *testing.T) {
	resetDefaultStore(t)

	loop, err := NewSubgraphOrganizer(Config{
		AgentID: "agent-a",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	store := ctxgraph.NewStore()
	bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n-copy",
			Statement:   "from copy",
			SubgraphIDs: []string{"sg-a"},
		}},
	})
	ctxgraph.Update(ctxgraph.Copy{
		AgentID: "other",
		Graph: ctxgraph.Graph{
			Nodes: []ctxgraph.Node{{
				ID:          "n-global",
				Statement:   "from global",
				SubgraphIDs: []string{"sg-a"},
			}},
		},
	})

	tool, ok := loop.tools["memory_nodes_in"]
	if !ok {
		t.Fatal("memory_nodes_in not registered")
	}
	out, err := tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      "memory_nodes_in",
		Arguments: json.RawMessage(`{"subgraph_ids":["sg-a"]}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(out.Content), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.Content, err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "n-copy" {
		t.Fatalf("nodes = %#v, want [n-copy] from the agent copy", got.Nodes)
	}
}

func TestOrganizeSubgraphToolAsksOrganizer(t *testing.T) {
	resetDefaultStore(t)

	var query string
	collector := event.NewCollector()
	events := event.NewBus(collector.Handle)
	organizer, err := NewSubgraphOrganizer(Config{
		Events: events,
		Provider: ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			if len(request.Messages) > 0 {
				query = request.Messages[0].Content
			}
			return AssistantMessage{Content: "ok"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	requester, err := NewLoop(Config{
		AgentID: "requester",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
		Tools: []agenttool.Tool{OrganizeSubgraphTool(organizer)},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	bindEnvGraph(t, requester, store, "env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{
			ID:   "sg-seed",
			Name: "seed",
			Kind: ctxgraph.SubgraphKindGeneral,
		}},
		Nodes: []ctxgraph.Node{{
			ID:          "n-seed",
			Statement:   "user prefers blue preference",
			SubgraphIDs: []string{"sg-seed"},
		}},
	})
	requester.SetSubscribedSubgraphs([]string{"sg-old"})

	tool, ok := requester.tools["organize_subgraph"]
	if !ok {
		t.Fatal("organize_subgraph not registered on requester")
	}
	if err := tool.Definition().Validate(); err != nil {
		t.Fatalf("Definition().Validate() = %v", err)
	}

	out, err := tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      "organize_subgraph",
		Arguments: json.RawMessage(`{"query":"blue preference"}`),
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var result organizeSubgraphResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("decode result %q: %v", out.Content, err)
	}
	subgraph := result.Subgraph
	if subgraph.ID == "" || subgraph.Kind != ctxgraph.SubgraphKindTask {
		t.Fatalf("subgraph = %#v", subgraph)
	}
	if subgraph.Name != "blue preference" || subgraph.Summary != "blue preference" {
		t.Fatalf("subgraph labels = %#v", subgraph)
	}
	if !strings.Contains(query, "blue preference") || !strings.Contains(query, subgraph.ID) {
		t.Fatalf("organizer query = %q, want original query and subgraph id", query)
	}
	if !strings.Contains(query, "n-seed") || !strings.Contains(query, "sg-seed") {
		t.Fatalf("organizer query = %q, want existing subgraph and matching node ids", query)
	}
	if got, want := requester.subscribedSubgraphs, []string{"sg-old", subgraph.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions = %v, want %v", got, want)
	}
	metrics := collector.Snapshot()
	if metrics.MemoryOrganizerRuns != 1 ||
		metrics.MemoryOrganizerCandidates != 1 ||
		metrics.MemoryOrganizerSelected != 0 {
		t.Fatalf("organizer metrics = %#v", metrics)
	}
}

func TestOrganizeQueryListsTokenMatchedNodes(t *testing.T) {
	t.Parallel()

	got := organizeQuery("THREADMILL_GRAPH_MEM_7f3a 核验", "", "sg-q-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-seed", Kind: ctxgraph.SubgraphKindGeneral}},
		Nodes: []ctxgraph.Node{{
			ID:        "n-seed",
			Statement: "user preference marker THREADMILL_GRAPH_MEM_7f3a",
		}},
	}, "")
	if !strings.Contains(got, "n-seed") || !strings.Contains(got, "sg-seed") {
		t.Fatalf("organizeQuery() = %q, want token match n-seed and subgraph catalog", got)
	}
}

func TestOrganizeSubgraphToolMissingQuery(t *testing.T) {
	tool := OrganizeSubgraphTool(&Loop{})
	_, err := tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      "organize_subgraph",
		Arguments: json.RawMessage(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("Execute() error = %v, want missing query", err)
	}
}

func TestRoleAgentsUseMemoryHooksAndRolePrompt(t *testing.T) {
	echo := &testTool{
		definition: agenttool.Definition{
			Name:        "echo",
			Description: "Echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			return agenttool.Output{Content: "ok"}, nil
		},
	}
	tests := []struct {
		name    string
		id      string
		prompt  string
		newLoop func(Config) (*Loop, error)
	}{
		{"planner", plannerID, "yaml planner", NewPlanner},
		{"executor", executorID, "yaml executor", NewExecutor},
		{"verifier", verifierID, "yaml verifier", NewVerifier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDefaultStore(t)

			var request Request
			loop, err := tt.newLoop(Config{
				Provider: ignoreOrganize(func(_ context.Context, got Request) (AssistantMessage, error) {
					request = got
					return AssistantMessage{Content: "done"}, nil
				}),
				Tools:        []agenttool.Tool{echo},
				SystemPrompt: tt.prompt,
			})
			if err != nil {
				t.Fatal(err)
			}
			if loop.agentID != tt.id {
				t.Fatalf("agent id = %q, want %q", loop.agentID, tt.id)
			}
			if len(loop.hooks.AssembleRequest) == 0 ||
				len(loop.hooks.AfterAssistant) == 0 ||
				len(loop.hooks.CommitTurn) == 0 {
				t.Fatal("memory hooks not registered")
			}

			loop.SetSubscribedSubgraphs([]string{"sg-a"})

			store := ctxgraph.NewStore()
			bindEnvGraph(t, loop, store, "env-1", ctxgraph.Graph{
				Nodes: []ctxgraph.Node{{
					ID:          "n1",
					Statement:   "shared fact",
					SubgraphIDs: []string{"sg-a"},
				}},
			})

			answer, err := loop.Ask(context.Background(), "start")
			if err != nil {
				t.Fatalf("Ask() error = %v", err)
			}
			if answer != "done" {
				t.Fatalf("Ask() = %q, want done", answer)
			}

			wantMemory := "记忆：\n- shared fact"
			if got := blockText(request, "memory"); got != wantMemory {
				t.Fatalf("memory block = %q, want %q", got, wantMemory)
			}
			if request.SystemPrompt != tt.prompt {
				t.Fatalf("system prompt = %q, want %q", request.SystemPrompt, tt.prompt)
			}

			foundEcho := false
			foundOrganize := false
			for _, def := range request.Tools {
				switch def.Name {
				case "echo":
					foundEcho = true
				case organizeSubgraphToolName:
					foundOrganize = true
				}
			}
			if !foundEcho || !foundOrganize {
				t.Fatalf("request tools = %#v, want echo and %s", request.Tools, organizeSubgraphToolName)
			}

			tool, ok := loop.tools[organizeSubgraphToolName]
			if !ok {
				t.Fatal("organize_subgraph not registered")
			}
			out, err := tool.Execute(context.Background(), agenttool.Call{
				ID:        "call-org-1",
				Name:      organizeSubgraphToolName,
				Arguments: json.RawMessage(`{"query":"blue preference"}`),
			})
			if err != nil {
				t.Fatalf("organize_subgraph Execute() error = %v", err)
			}
			var result organizeSubgraphResult
			if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
				t.Fatalf("decode result %q: %v", out.Content, err)
			}
			subgraph := result.Subgraph
			if subgraph.ID == "" {
				t.Fatalf("subgraph = %#v", subgraph)
			}
			wantSubs := []string{"sg-a", subgraph.ID}
			if got := loop.subscribedSubgraphs; !reflect.DeepEqual(got, wantSubs) {
				t.Fatalf("subscriptions = %v, want %v", got, wantSubs)
			}
		})
	}
}

func TestNewPlannerBindAllowsAsk(t *testing.T) {
	resetDefaultStore(t)
	loop, err := NewPlanner(Config{
		Provider: ignoreOrganize(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	if err := loop.Bind(env.Open("env-1", store.View("env-1"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	answer, err := loop.Ask(context.Background(), "start")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
}

func TestNewPlannerKeepsConfiguredAgentID(t *testing.T) {
	resetDefaultStore(t)
	loop, err := NewPlanner(Config{
		AgentID: "custom-planner",
		Provider: modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "unused"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if loop.agentID != "custom-planner" {
		t.Fatalf("agent id = %q, want custom-planner", loop.agentID)
	}
}

func TestNewTeamRegistersFileTools(t *testing.T) {
	resetDefaultStore(t)

	team, err := NewTeam(
		modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{
				Tools: []string{fileReadToolName, fileLsToolName, fileGrepToolName, fileFindToolName},
			},
			Executor: FileAgent{
				Tools: []string{fileReadToolName, fileWriteToolName, fileEditToolName, bashToolName},
			},
			Verifier: FileAgent{
				Tools: []string{fileReadToolName, fileLsToolName, fileGrepToolName, fileFindToolName},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{fileReadToolName, fileLsToolName, fileGrepToolName, fileFindToolName} {
		if _, ok := team.Planner.tools[name]; !ok {
			t.Fatalf("planner missing %s", name)
		}
		if _, ok := team.Verifier.tools[name]; !ok {
			t.Fatalf("verifier missing %s", name)
		}
	}
	for _, name := range []string{fileReadToolName, fileWriteToolName, fileEditToolName, bashToolName} {
		if _, ok := team.Executor.tools[name]; !ok {
			t.Fatalf("executor missing %s", name)
		}
	}
	if _, ok := team.Planner.tools[bashToolName]; ok {
		t.Fatal("planner gained bash")
	}
	if _, ok := team.Verifier.tools[bashToolName]; ok {
		t.Fatal("verifier gained bash")
	}
}

func TestNewTeamDoesNotInjectRolePrompt(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	team, err := NewTeam(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if strings.Contains(request.SystemPrompt, "规划 Agent") {
		t.Fatalf("factory injected planner prompt: %q", request.SystemPrompt)
	}
}

func TestNewTeamUsesYamlDefaultPromptWhenRolePromptEmpty(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	team, err := NewTeam(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{},
		nil,
		FileOverlay{Prompts: FilePrompts{Default: "yaml default"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if request.SystemPrompt != "yaml default" {
		t.Fatalf("system prompt = %q, want yaml default", request.SystemPrompt)
	}
}

func TestNewTeamUsesFileAgentsAndSharesOrganizer(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	model := ignoreOrganize(func(_ context.Context, got Request) (AssistantMessage, error) {
		request = got
		return AssistantMessage{Content: "done"}, nil
	})
	team, err := NewTeam(
		model,
		9000,
		FileAgents{
			Planner: FileAgent{
				ID:           "yaml-planner",
				SystemPrompt: "yaml plan",
				Tools:        []string{organizeSubgraphToolName},
				Hooks: []string{
					hookInjectSubscribedMemory,
					hookCompactOnOverflow,
					hookCommitTailOnTurnEnd,
				},
			},
			Executor: FileAgent{
				ID:           "yaml-executor",
				SystemPrompt: "yaml execute",
				Tools:        []string{organizeSubgraphToolName},
			},
			Verifier: FileAgent{
				ID:           "yaml-verifier",
				SystemPrompt: "yaml verify",
				Tools:        []string{organizeSubgraphToolName},
			},
			SubgraphOrganizer: FileAgent{
				ID:           "yaml-organizer",
				SystemPrompt: "yaml organize",
				Tools: []string{
					"memory_neighbors",
					"memory_nodes_in",
					memoryDropFromContextToolName,
				},
				Hooks: []string{hookRemindDropContextOnPressure},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if team.Planner.agentID != "yaml-planner" ||
		team.Executor.agentID != "yaml-executor" ||
		team.Verifier.agentID != "yaml-verifier" ||
		team.Organizer.agentID != "yaml-organizer" {
		t.Fatalf("ids = %q %q %q %q",
			team.Planner.agentID,
			team.Executor.agentID,
			team.Verifier.agentID,
			team.Organizer.agentID,
		)
	}

	if tool, ok := team.Planner.tools[organizeSubgraphToolName].(*organizeSubgraphTool); !ok {
		t.Fatal("planner organize_subgraph is not the organizer tool")
	} else if tool.organizer != team.Organizer {
		t.Fatal("planner does not share the yaml subgraph organizer")
	}
	if team.Planner.contextWindow != 9000 ||
		team.Executor.contextWindow != 9000 ||
		team.Verifier.contextWindow != 9000 ||
		team.Organizer.contextWindow != 9000 {
		t.Fatalf(
			"context windows = %d %d %d %d, want 9000 from the model",
			team.Planner.contextWindow,
			team.Executor.contextWindow,
			team.Verifier.contextWindow,
			team.Organizer.contextWindow,
		)
	}
	if len(team.Planner.hooks.AssembleRequest) == 0 ||
		len(team.Planner.hooks.AfterAssistant) == 0 ||
		len(team.Planner.hooks.CommitTurn) == 0 {
		t.Fatal("planner yaml hooks not registered")
	}
	if len(team.Executor.hooks.AssembleRequest) != 0 {
		t.Fatal("executor listed no hooks, want none")
	}
	if _, ok := team.Organizer.tools["memory_neighbors"]; !ok {
		t.Fatal("organizer yaml tools missing memory_neighbors")
	}
	if _, ok := team.Organizer.tools["memory_add_to_subgraph"]; ok {
		t.Fatal("organizer listed only a subset of memory tools")
	}
	if _, ok := team.Organizer.tools[memoryDropFromContextToolName]; !ok {
		t.Fatal("organizer yaml tools missing memory_drop_from_context")
	}
	if len(team.Organizer.hooks.AssembleRequest) == 0 {
		t.Fatal("organizer yaml drop-context reminder not registered")
	}

	store := ctxgraph.NewStore()
	if err := team.Bind(env.Open("env-1", store.View("env-1"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	team.BindCheckpoints(nil, "task-42")
	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Planner.Ask() error = %v", err)
	}
	if request.SystemPrompt != "yaml plan" {
		t.Fatalf("planner prompt = %q, want yaml plan", request.SystemPrompt)
	}
	if request.CacheKey != "yaml-planner" {
		t.Fatalf("planner cache key = %q, want stable role key yaml-planner", request.CacheKey)
	}
}

func TestTeamBindUsesYamlPluginsAgainstEnvStore(t *testing.T) {
	resetDefaultStore(t)

	store := ctxgraph.NewStore()
	store.Save("env-1", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "bound"}},
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "local fact",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg-a"},
		}},
	})
	ctxgraph.Update(ctxgraph.Copy{
		Graph: ctxgraph.Graph{
			Nodes: []ctxgraph.Node{{
				ID:          "n1",
				Kind:        ctxgraph.NodeKindFact,
				Statement:   "global fact",
				Status:      ctxgraph.NodeStatusAccepted,
				SubgraphIDs: []string{"sg-a"},
			}},
		},
	})

	var request Request
	team, err := NewTeam(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			if hasToolResultMessages(got.Messages) {
				return AssistantMessage{Content: "done"}, nil
			}
			return AssistantMessage{
				ToolCalls: []agenttool.Call{{
					ID:        "call-1",
					Name:      memoryAddToSubgraphToolName,
					Arguments: json.RawMessage(`{"subgraph_id":"bound","node_ids":["n1"]}`),
				}},
			}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{
				SystemPrompt: "yaml plan",
				Tools:        []string{organizeSubgraphToolName, memoryAddToSubgraphToolName},
				Hooks:        []string{hookInjectSubscribedMemory},
			},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := team.Bind(env.Open("env-1", store.View("env-1"))); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	team.Planner.SetSubscribedSubgraphs([]string{"sg-a"})

	answer, err := team.Planner.Ask(context.Background(), "start")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if answer != "done" {
		t.Fatalf("Ask() = %q, want done", answer)
	}
	if !strings.Contains(request.SystemPrompt, "yaml plan") {
		t.Fatalf("planner prompt = %q, want yaml plan", request.SystemPrompt)
	}
	if !strings.Contains(blockText(request, "memory"), "local fact") {
		t.Fatalf("planner request missing env memory: %#v", request.Messages)
	}
	if strings.Contains(blockText(request, "memory"), "global fact") {
		t.Fatalf("planner request used global memory: %#v", request.Messages)
	}
	if !hasRequestTool(request.Tools, organizeSubgraphToolName) {
		t.Fatal("yaml organize_subgraph missing")
	}
	if nodes := ctxgraph.Clone("check").Graph.NodesInSubgraphs([]string{"bound"}); len(nodes) != 0 {
		t.Fatalf("write leaked to global graph: %#v", nodes)
	}
	if nodes := store.Load("env-1").NodesInSubgraphs([]string{"bound"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("write did not stay in env-1")
	}
}

func TestFileAgentsRejectsManagerGraphToolsOnPlanner(t *testing.T) {
	for _, toolName := range []string{coordOrchestrateToolName, coordPublishTaskToolName} {
		err := FileAgents{
			Planner: FileAgent{Tools: []string{toolName}},
		}.Validate()
		if err == nil || !strings.Contains(err.Error(), "manager-only") {
			t.Fatalf("Validate(%s) error = %v, want manager-only", toolName, err)
		}
	}
}

func TestFileAgentsRejectsJoinOutsideTaskRoles(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"manager", "subgraph_organizer"} {
		role := role
		t.Run(role, func(t *testing.T) {
			agents := FileAgents{}
			if role == "manager" {
				agents.Manager.Tools = []string{coordJoinToolName}
			} else {
				agents.SubgraphOrganizer.Tools = []string{coordJoinToolName}
			}
			if err := agents.Validate(); err == nil || !strings.Contains(err.Error(), "task-role-only") {
				t.Fatalf("Validate() error = %v, want task-role-only", err)
			}
		})
	}
}

func TestNewManagerInstallsGraphTools(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	graphTool := &testTool{
		definition: agenttool.Definition{
			Name:        coordOrchestrateToolName,
			Description: "replace pending",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		execute: func(context.Context, agenttool.Call) (agenttool.Output, error) {
			return agenttool.Output{Content: "{}"}, nil
		},
	}
	loop, err := NewManager(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{
			Manager: FileAgent{
				SystemPrompt: "yaml manager",
				Tools:        []string{coordOrchestrateToolName, organizeSubgraphToolName},
			},
		},
		nil,
		FileOverlay{
			NamedTools: map[string]agenttool.Tool{
				coordOrchestrateToolName: graphTool,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loop.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if request.SystemPrompt != "yaml manager" {
		t.Fatalf("system prompt = %q, want yaml manager", request.SystemPrompt)
	}
	if !hasRequestTool(request.Tools, coordOrchestrateToolName) {
		t.Fatal("manager missing coordination_orchestrate")
	}
	if !hasRequestTool(request.Tools, organizeSubgraphToolName) {
		t.Fatal("manager missing organize_subgraph")
	}
}

func TestNewTeamDoesNotInstallGraphTools(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	team, err := NewTeam(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{Tools: []string{organizeSubgraphToolName}},
		},
		nil,
		FileOverlay{
			NamedTools: map[string]agenttool.Tool{
				coordOrchestrateToolName: &testTool{
					definition: agenttool.Definition{
						Name:        coordOrchestrateToolName,
						Description: "replace pending",
						InputSchema: json.RawMessage(`{"type":"object"}`),
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if hasRequestTool(request.Tools, coordOrchestrateToolName) {
		t.Fatal("planner gained coordination_orchestrate")
	}
}

func TestNewManagerRequiresNamedGraphTools(t *testing.T) {
	resetDefaultStore(t)

	_, err := NewManager(
		modelFunc(func(context.Context, Request) (AssistantMessage, error) {
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{
			Manager: FileAgent{Tools: []string{coordOrchestrateToolName}},
		},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "coordination_orchestrate"`) {
		t.Fatalf("NewManager() error = %v, want unknown coordination_orchestrate", err)
	}
}

func TestNewTeamAppliesYamlToolDescription(t *testing.T) {
	resetDefaultStore(t)

	var request Request
	team, err := NewTeam(
		modelFunc(func(_ context.Context, got Request) (AssistantMessage, error) {
			request = got
			return AssistantMessage{Content: "done"}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{Tools: []string{fileReadToolName}},
		},
		nil,
		FileOverlay{
			Tools: FileToolCatalog{
				fileReadToolName: {Description: "yaml read intro"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	got := ""
	for _, tool := range request.Tools {
		if tool.Name == fileReadToolName {
			got = tool.Description
			break
		}
	}
	if got != "yaml read intro" {
		t.Fatalf("read description = %q, want yaml read intro", got)
	}
}

func TestNewTeamAppliesYamlDropContextReminder(t *testing.T) {
	resetDefaultStore(t)

	var suffix string
	team, err := NewTeam(
		modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
			suffix = request.Suffix
			return AssistantMessage{Content: "done"}, nil
		}),
		40,
		FileAgents{
			SubgraphOrganizer: FileAgent{
				SystemPrompt: "org",
				Hooks:        []string{hookRemindDropContextOnPressure},
			},
		},
		nil,
		FileOverlay{
			Prompts: FilePrompts{DropContextPressure: "yaml drop reminder"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := team.Organizer.Ask(context.Background(), strings.Repeat("n", 400)); err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if !strings.Contains(suffix, "yaml drop reminder") {
		t.Fatalf("suffix = %q, want yaml drop reminder", suffix)
	}
	if strings.Contains(suffix, dropContextPressureReminder) {
		t.Fatalf("suffix = %q, want yaml reminder instead of default", suffix)
	}
}

func TestNewTeamAppliesYamlOrganizeQuery(t *testing.T) {
	resetDefaultStore(t)

	var query string
	team, err := NewTeam(
		ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			if len(request.Messages) > 0 {
				query = request.Messages[0].Content
			}
			return AssistantMessage{Content: "ok"}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{Tools: []string{organizeSubgraphToolName}},
			SubgraphOrganizer: FileAgent{
				Tools: []string{memoryNeighborsToolName},
			},
		},
		nil,
		FileOverlay{
			Prompts: FilePrompts{OrganizeQuery: "yaml organize query"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	bindEnvGraph(t, team.Planner, store, "env-1", ctxgraph.Graph{})

	tool, ok := team.Planner.tools[organizeSubgraphToolName]
	if !ok {
		t.Fatal("organize_subgraph missing")
	}
	if _, err := tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      organizeSubgraphToolName,
		Arguments: json.RawMessage(`{"query":"blue"}`),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(query, "yaml organize query") {
		t.Fatalf("organizer query = %q, want yaml organize query", query)
	}
}

func TestNewTeamDoesNotInjectOrganizeQuery(t *testing.T) {
	resetDefaultStore(t)

	var query string
	team, err := NewTeam(
		ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
			if len(request.Messages) > 0 {
				query = request.Messages[0].Content
			}
			return AssistantMessage{Content: "ok"}, nil
		}),
		0,
		FileAgents{
			Planner: FileAgent{Tools: []string{organizeSubgraphToolName}},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := ctxgraph.NewStore()
	bindEnvGraph(t, team.Planner, store, "env-1", ctxgraph.Graph{})

	tool, ok := team.Planner.tools[organizeSubgraphToolName]
	if !ok {
		t.Fatal("organize_subgraph missing")
	}
	if _, err := tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      organizeSubgraphToolName,
		Arguments: json.RawMessage(`{"query":"blue"}`),
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(query, "请把相关节点加入这个子图") {
		t.Fatalf("organizer query injected factory prompt: %q", query)
	}
}

func hasRequestTool(tools []agenttool.Definition, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func hasToolResultMessages(messages []Message) bool {
	for _, message := range messages {
		if message.Role == RoleTool {
			return true
		}
	}
	return false
}

func TestCountQueryMatchesCountsTheListedCandidates(t *testing.T) {
	t.Parallel()

	graph := ctxgraph.Graph{Nodes: []ctxgraph.Node{
		{ID: "mem-1", Statement: "CPack source archive 仍含 build-aux"},
		{ID: "mem-2", Statement: "CTest 默认注册 80 个测试"},
		{ID: "mem-3", Statement: "无关节点"},
	}}
	query := "召回 CPack source archive 的历史证据"

	if got := countQueryMatches(graph, query); got != 1 {
		t.Fatalf("candidates = %d, want only the nodes the query message lists", got)
	}
}
