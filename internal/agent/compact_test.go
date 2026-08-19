package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

type stubProvider struct {
	response string
	err      error
	last     Request
	calls    int
}

func (s *stubProvider) Generate(_ context.Context, request Request) (AssistantMessage, error) {
	s.calls++
	s.last = cloneRequest(request)
	if s.err != nil {
		return AssistantMessage{}, s.err
	}
	return AssistantMessage{Content: s.response}, nil
}

func compactPromptCtx(prompt, reminder string) context.Context {
	return WithTranscript(context.Background(), Transcript{
		CompactPrompt:       prompt,
		CompactJSONReminder: reminder,
	})
}

func TestCompactHistoryUsesOneModelRequestAndKeepsTail(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{
  "nodes": [
    {
      "kind": "fact",
      "statement": "user prefers blue",
      "status": "accepted",
      "subgraph_ids": ["sg-a"]
    }
  ]
}`}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
		Nodes: []ctxgraph.Node{{
			ID:             "old",
			Statement:      "already known",
			SubgraphIDs:    []string{"sg-a"},
			CreatorAgentID: "agent-a",
		}},
	}
	messages := []Message{
		{Role: RoleUser, Content: "old work, I like blue"},
		{Role: RoleAssistant, Content: "noted, using blue"},
		{Role: RoleAssistant, Content: "hello"},
	}

	gotGraph, tail, err := CompactHistory(
		compactPromptCtx("yaml compact", "json only"),
		provider,
		graph,
		messages,
		[]string{"sg-a"},
		1,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("model calls = %d, want 1", provider.calls)
	}
	if provider.last.SystemPrompt != "yaml compact" {
		t.Fatalf("system prompt = %q, want yaml compact", provider.last.SystemPrompt)
	}
	if len(provider.last.Tools) != 0 {
		t.Fatalf("tools = %#v, want none", provider.last.Tools)
	}
	if len(provider.last.Messages) != 1 {
		t.Fatalf("organize messages = %#v, want one user payload", provider.last.Messages)
	}
	payload := provider.last.Messages[0].Content
	if !strings.Contains(payload, "[User]: old work, I like blue") {
		t.Fatalf("payload = %q, want serialized prefix", payload)
	}
	if strings.Contains(payload, "[Assistant]: hello") {
		t.Fatal("payload included the kept tail")
	}

	if len(tail) != 1 || tail[0].Content != "hello" {
		t.Fatalf("tail = %#v, want the last assistant message", tail)
	}

	nodes := gotGraph.NodesInSubgraphs([]string{"sg-a"})
	if len(nodes) != 2 || nodes[1].Statement != "user prefers blue" {
		t.Fatalf("subgraph nodes = %#v, want one model-made node", nodes)
	}
	if nodes[1].Kind != ctxgraph.NodeKindFact || nodes[1].Status != ctxgraph.NodeStatusAccepted {
		t.Fatalf("node kind/status = %#v", nodes[1])
	}
	if nodes[1].CreatorAgentID != "agent-a" {
		t.Fatalf("creator = %q, want agent-a", nodes[1].CreatorAgentID)
	}
	if got := gotGraph.SourceSubgraphsOf(nodes[1].ID); !reflect.DeepEqual(got, []string{"sg-a"}) {
		t.Fatalf("source subgraphs = %v, want [sg-a]", got)
	}
	upstream := gotGraph.UpstreamNodes(nodes[1].ID)
	if len(upstream) != 1 || upstream[0].ID != "old" {
		t.Fatalf("previous node = %#v, want old", upstream)
	}
}

func TestCompactHistoryDoesNotInjectBuiltInPrompt(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[]}`}
	_, _, err := CompactHistory(
		context.Background(),
		provider,
		ctxgraph.Graph{},
		[]Message{
			{Role: RoleUser, Content: "old work"},
			{Role: RoleAssistant, Content: "noted"},
		},
		nil,
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.last.SystemPrompt != "" {
		t.Fatalf("system prompt = %q, want empty without yaml transcript", provider.last.SystemPrompt)
	}
}

func TestCompactHistoryAssignsMembershipFromModelAndSourcesFromSubscriptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		modelIDs   []string
		wantMember []string
	}{
		{
			name:       "model chooses membership",
			modelIDs:   []string{"sg-b"},
			wantMember: []string{"sg-b"},
		},
		{
			name:       "unknown membership stays empty",
			modelIDs:   []string{"sg-missing"},
			wantMember: []string{},
		},
		{
			name:       "empty membership stays empty",
			modelIDs:   []string{},
			wantMember: []string{},
		},
	}

	subscribed := []string{"sg-a", "sg-q"}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask},
			{ID: "sg-b", Kind: ctxgraph.SubgraphKindGeneral},
			{ID: "sg-q", Kind: ctxgraph.SubgraphKindTask},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ids, err := json.Marshal(tt.modelIDs)
			if err != nil {
				t.Fatal(err)
			}
			provider := &stubProvider{response: `{"nodes":[{"kind":"fact","statement":"user prefers blue","status":"accepted","subgraph_ids":` + string(ids) + `}]}`}
			gotGraph, _, err := CompactHistory(
				context.Background(),
				provider,
				graph,
				[]Message{
					{Role: RoleUser, Content: "old work, I like blue"},
					{Role: RoleAssistant, Content: "noted"},
				},
				subscribed,
				0,
				"agent-a",
			)
			if err != nil {
				t.Fatal(err)
			}

			var created ctxgraph.Node
			for _, node := range gotGraph.Nodes {
				if node.Statement == "user prefers blue" {
					created = node
					break
				}
			}
			if created.ID == "" {
				t.Fatal("missing compacted node")
			}
			if len(created.SubgraphIDs) != len(tt.wantMember) {
				t.Fatalf("membership = %v, want %v", created.SubgraphIDs, tt.wantMember)
			}
			for i, id := range tt.wantMember {
				if created.SubgraphIDs[i] != id {
					t.Fatalf("membership = %v, want %v", created.SubgraphIDs, tt.wantMember)
				}
			}
			if got := gotGraph.SourceSubgraphsOf(created.ID); !reflect.DeepEqual(got, subscribed) {
				t.Fatalf("source subgraphs = %v, want subscriptions %v", got, subscribed)
			}
			if !slices.Contains(tt.wantMember, "sg-a") {
				if nodes := gotGraph.NodesInSubgraphs([]string{"sg-a"}); len(nodes) != 0 {
					t.Fatalf("subscribed subgraph gained membership %#v", nodes)
				}
			}
		})
	}
}

func TestParseOrganizeOutputAcceptsFencedJSON(t *testing.T) {
	t.Parallel()

	got, err := parseOrganizeOutput("```json\n{\"nodes\":[{\"kind\":\"hypothesis\",\"statement\":\"maybe later\",\"status\":\"disputed\"}]}\n```")
	if err != nil {
		t.Fatal(err)
	}
	want := []organizeNode{{
		Kind:      ctxgraph.NodeKindHypothesis,
		Statement: "maybe later",
		Status:    ctxgraph.NodeStatusDisputed,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseOrganizeOutput() = %#v, want %#v", got, want)
	}
}

func TestKeepRecentIndex(t *testing.T) {
	t.Parallel()

	user := Message{Role: RoleUser, Content: "start"}
	assistant := Message{Role: RoleAssistant, Content: "hello"}
	tool := Message{Role: RoleTool, Content: "tool-output"}
	final := Message{Role: RoleAssistant, Content: "done"}

	tests := []struct {
		name     string
		messages []Message
		keep     int
		expected int
	}{
		{
			name:     "commit all when keep is zero",
			messages: []Message{user, assistant},
			keep:     0,
			expected: 2,
		},
		{
			name:     "keep everything when under budget",
			messages: []Message{user, assistant},
			keep:     1000,
			expected: 0,
		},
		{
			name:     "cut at last assistant when it fills the budget",
			messages: []Message{user, assistant, tool, final},
			keep:     1,
			expected: 3,
		},
		{
			name:     "do not cut at a tool result",
			messages: []Message{user, assistant, tool},
			keep:     estimateTokens(tool),
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := keepRecentIndex(tt.messages, tt.keep); got != tt.expected {
				t.Fatalf("keepRecentIndex() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestCompactHistoryCompactsWhenNoSubscriptions(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[{"kind":"fact","statement":"filed away","status":"accepted","subgraph_ids":[]}]}`}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
	}
	messages := []Message{
		{Role: RoleUser, Content: "old work"},
		{Role: RoleAssistant, Content: "noted"},
	}
	gotGraph, tail, err := CompactHistory(
		context.Background(),
		provider,
		graph,
		messages,
		nil,
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("model calls = %d, want 1", provider.calls)
	}
	if len(tail) != 0 {
		t.Fatalf("tail = %#v, want empty after keep=0", tail)
	}
	if len(gotGraph.Nodes) != 1 || gotGraph.Nodes[0].Statement != "filed away" {
		t.Fatalf("nodes = %#v, want compacted node", gotGraph.Nodes)
	}
	if len(gotGraph.Nodes[0].SubgraphIDs) != 0 {
		t.Fatalf("membership = %v, want empty", gotGraph.Nodes[0].SubgraphIDs)
	}
	if nodes := gotGraph.NodesInSubgraphs([]string{"sg-a"}); len(nodes) != 0 {
		t.Fatalf("unsubscribed window saw compacted node: %#v", nodes)
	}
}

func TestCompactHistoryPromptListsAllSubgraphsAndOnlySubscribedMemory(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[]}`}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask, Name: "mine"},
			{ID: "sg-b", Kind: ctxgraph.SubgraphKindGeneral, Name: "other"},
		},
		Nodes: []ctxgraph.Node{
			{ID: "keep", Statement: "visible fact", SubgraphIDs: []string{"sg-a"}},
			{ID: "hide", Statement: "secret other", SubgraphIDs: []string{"sg-b"}},
		},
	}
	_, _, err := CompactHistory(
		context.Background(),
		provider,
		graph,
		[]Message{
			{Role: RoleUser, Content: "old work"},
			{Role: RoleAssistant, Content: "noted"},
		},
		[]string{"sg-a"},
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	payload := provider.last.Messages[0].Content
	if !strings.Contains(payload, "sg-b") {
		t.Fatalf("payload = %q, want all subgraphs in catalog", payload)
	}
	if !strings.Contains(payload, "visible fact") {
		t.Fatalf("payload = %q, want subscribed memory", payload)
	}
	if strings.Contains(payload, "secret other") {
		t.Fatal("organize prompt leaked unsubscribed node statement")
	}
}

func TestCompactHistoryLinksCreatorChainNotForeignNodes(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[{"kind":"fact","statement":"from a","status":"accepted","subgraph_ids":["sg-a"]}]}`}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
		Nodes: []ctxgraph.Node{
			{ID: "other", Statement: "from b", CreatorAgentID: "agent-b", SubgraphIDs: []string{"sg-a"}},
			{ID: "mine", Statement: "earlier a", CreatorAgentID: "agent-a", SubgraphIDs: []string{"sg-a"}},
		},
	}
	gotGraph, _, err := CompactHistory(
		context.Background(),
		provider,
		graph,
		[]Message{
			{Role: RoleUser, Content: "old work"},
			{Role: RoleAssistant, Content: "noted"},
		},
		[]string{"sg-a"},
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	var created ctxgraph.Node
	for _, node := range gotGraph.Nodes {
		if node.Statement == "from a" {
			created = node
			break
		}
	}
	if created.ID == "" {
		t.Fatal("missing compacted node")
	}
	upstream := gotGraph.UpstreamNodes(created.ID)
	if len(upstream) != 1 || upstream[0].ID != "mine" {
		t.Fatalf("previous node = %#v, want mine", upstream)
	}
}

func TestCompactHistoryReturnsProviderError(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{err: errors.New("boom")}
	_, _, err := CompactHistory(
		context.Background(),
		provider,
		ctxgraph.Graph{},
		[]Message{{Role: RoleUser, Content: "x"}},
		[]string{"sg-a"},
		0,
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want organizing memory wrapping boom", err)
	}
	if provider.calls != 1 {
		t.Fatalf("model calls = %d, want 1 for provider errors", provider.calls)
	}
}

func TestCompactHistoryRetriesInvalidJSONWithReminder(t *testing.T) {
	t.Parallel()

	var second Request
	calls := 0
	provider := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{Content: `{"nodes":[{"kind":"fact"}`}, nil
		}
		second = cloneRequest(request)
		return AssistantMessage{Content: `{
  "nodes": [
    {
      "kind": "fact",
      "statement": "user prefers blue",
      "status": "accepted",
      "subgraph_ids": ["sg-a"]
    }
  ]
}`}, nil
	})

	gotGraph, _, err := CompactHistory(
		compactPromptCtx("yaml compact", "json only"),
		provider,
		ctxgraph.Graph{
			Subgraphs: []ctxgraph.Subgraph{{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask}},
		},
		[]Message{
			{Role: RoleUser, Content: "old work, I like blue"},
			{Role: RoleAssistant, Content: "noted, using blue"},
			{Role: RoleAssistant, Content: "hello"},
		},
		[]string{"sg-a"},
		1,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if second.SystemPrompt != "yaml compact" {
		t.Fatalf("retry system prompt = %q, want yaml compact", second.SystemPrompt)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("retry messages = %#v, want original user, bad assistant, reminder", second.Messages)
	}
	if second.Messages[1].Role != RoleAssistant || second.Messages[1].Content != `{"nodes":[{"kind":"fact"}` {
		t.Fatalf("retry assistant = %#v, want the invalid json", second.Messages[1])
	}
	if second.Messages[2].Role != RoleUser || !strings.Contains(second.Messages[2].Content, "json only") {
		t.Fatalf("retry reminder = %q, want yaml json reminder", second.Messages[2].Content)
	}

	nodes := gotGraph.NodesInSubgraphs([]string{"sg-a"})
	if len(nodes) != 1 || nodes[0].Statement != "user prefers blue" {
		t.Fatalf("subgraph nodes = %#v, want recovered fact", nodes)
	}
}

func TestSerializeConversationIncludesThinking(t *testing.T) {
	t.Parallel()

	got := serializeConversation([]Message{
		{Role: RoleUser, Content: "look up"},
		{
			Role:     RoleAssistant,
			Content:  "calling",
			Thinking: "need a tool",
			ToolCalls: []agenttool.Call{{
				Name:      "echo",
				Arguments: json.RawMessage(`{"text":"hi"}`),
			}},
		},
		{
			Role:    RoleTool,
			Content: "hi",
			ToolResult: &agenttool.Result{
				Content: "hi",
				Details: json.RawMessage(`{"secret":true}`),
			},
		},
	})
	want := "[User]: look up\n[Assistant thinking]: need a tool\n[Assistant]: calling\n[Assistant tool calls]: echo({\"text\":\"hi\"})\n[Tool result]: hi"
	if got != want {
		t.Fatalf("serializeConversation() = %q, want %q", got, want)
	}
	if strings.Contains(got, "secret") {
		t.Fatal("serialized conversation leaked tool details")
	}
}
