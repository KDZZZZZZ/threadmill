package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
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
	if request.SystemPrompt != SubgraphOrganizerPrompt {
		t.Fatalf("system prompt = %q, want organizer prompt", request.SystemPrompt)
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

	loop.SetContextGraph(ctxgraph.Graph{
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
	organizer, err := NewSubgraphOrganizer(Config{
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

	var subgraph ctxgraph.Subgraph
	if err := json.Unmarshal([]byte(out.Content), &subgraph); err != nil {
		t.Fatalf("decode subgraph %q: %v", out.Content, err)
	}
	if subgraph.ID == "" || subgraph.Kind != ctxgraph.SubgraphKindTask {
		t.Fatalf("subgraph = %#v", subgraph)
	}
	if subgraph.Name != "blue preference" || subgraph.Summary != "blue preference" {
		t.Fatalf("subgraph labels = %#v", subgraph)
	}
	if !strings.Contains(query, "blue preference") || !strings.Contains(query, subgraph.ID) {
		t.Fatalf("organizer query = %q, want original query and subgraph id", query)
	}
	if got, want := requester.subscribedSubgraphs, []string{"sg-old", subgraph.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions = %v, want %v", got, want)
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
		{"planner", plannerID, PlannerPrompt, NewPlanner},
		{"executor", executorID, ExecutorPrompt, NewExecutor},
		{"verifier", verifierID, VerifierPrompt, NewVerifier},
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
				Tools: []agenttool.Tool{echo},
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

			loop.SetContextGraph(ctxgraph.Graph{
				Nodes: []ctxgraph.Node{{
					ID:          "n1",
					Statement:   "shared fact",
					SubgraphIDs: []string{"sg-a"},
				}},
			})
			loop.SetSubscribedSubgraphs([]string{"sg-a"})

			answer, err := loop.Ask(context.Background(), "start")
			if err != nil {
				t.Fatalf("Ask() error = %v", err)
			}
			if answer != "done" {
				t.Fatalf("Ask() = %q, want done", answer)
			}

			want := tt.prompt + "\n\n记忆：\n- shared fact"
			if request.SystemPrompt != want {
				t.Fatalf("system prompt = %q, want %q", request.SystemPrompt, want)
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
			var subgraph ctxgraph.Subgraph
			if err := json.Unmarshal([]byte(out.Content), &subgraph); err != nil {
				t.Fatalf("decode subgraph %q: %v", out.Content, err)
			}
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

	if _, err := team.Planner.Ask(context.Background(), "start"); err != nil {
		t.Fatalf("Planner.Ask() error = %v", err)
	}
	if request.SystemPrompt != "yaml plan" {
		t.Fatalf("planner prompt = %q, want yaml plan", request.SystemPrompt)
	}

	tool, ok := team.Planner.tools[organizeSubgraphToolName].(*organizeSubgraphTool)
	if !ok {
		t.Fatal("planner organize_subgraph is not the organizer tool")
	}
	if tool.organizer != team.Organizer {
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
}
