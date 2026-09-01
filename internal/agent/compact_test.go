package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		CacheKey:            "executor",
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
	if provider.last.CacheKey != "executor:compact" {
		t.Fatalf("cache key = %q, want stable role key executor:compact", provider.last.CacheKey)
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
		{
			name:       "system membership is rejected",
			modelIDs:   []string{"sg-system"},
			wantMember: []string{},
		},
		{
			name:       "package membership is rejected",
			modelIDs:   []string{"task-package"},
			wantMember: []string{},
		},
	}

	subscribed := []string{"sg-a", "sg-q"}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask},
			{ID: "sg-b", Kind: ctxgraph.SubgraphKindGeneral},
			{ID: "sg-q", Kind: ctxgraph.SubgraphKindTask},
			{ID: "sg-system", Kind: ctxgraph.SubgraphKindSystem},
			{ID: "task-package", Kind: ctxgraph.SubgraphKindPackage},
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
	afterTool := Message{Role: RoleAssistant, Content: "x"}

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
		{
			name:     "compact an oversized completed tool exchange",
			messages: []Message{user, assistant, tool, afterTool},
			keep:     estimateTokens(tool),
			expected: 3,
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

func TestEstimateTokensUsesReplayModelDataInsteadOfDerivedAssistantFields(t *testing.T) {
	t.Parallel()

	message := Message{
		Role:      RoleAssistant,
		Content:   strings.Repeat("content", 100),
		Thinking:  strings.Repeat("thinking", 100),
		ModelData: json.RawMessage(`"` + strings.Repeat("x", 400) + `"`),
		ToolCalls: []agenttool.Call{{
			Name:      "echo",
			Arguments: json.RawMessage(`{"text":"ignored when replay data exists"}`),
		}},
	}
	want := (len(message.ModelData) + 3) / 4
	if got := estimateTokens(message); got != want {
		t.Fatalf("estimateTokens() = %d, want %d from replay model data", got, want)
	}
}

func TestEstimateTokensCountsToolResultOnce(t *testing.T) {
	t.Parallel()

	result := &agenttool.Result{
		CallID:  "call-1",
		Name:    "bash",
		Content: strings.Repeat("output", 100),
		Details: json.RawMessage(`"` + strings.Repeat("internal", 100) + `"`),
	}
	message := Message{
		Role:       RoleTool,
		Content:    result.Content,
		ToolResult: result,
	}
	wantChars := len(result.CallID) + len(result.Name) + len(result.Content)
	want := (wantChars + 3) / 4
	if got := estimateTokens(message); got != want {
		t.Fatalf("estimateTokens() = %d, want %d from replayed tool result", got, want)
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
			{ID: "sg-system", Kind: ctxgraph.SubgraphKindSystem, Name: "managed"},
			{ID: "task-package", Kind: ctxgraph.SubgraphKindPackage, Name: "startup"},
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
	if strings.Contains(payload, "sg-system") {
		t.Fatalf("payload = %q, system subgraph must not be model-selectable", payload)
	}
	if strings.Contains(payload, "task-package") {
		t.Fatalf("payload = %q, package subgraph must not be model-selectable", payload)
	}
	if !strings.Contains(payload, "visible fact") {
		t.Fatalf("payload = %q, want subscribed memory", payload)
	}
	if strings.Contains(payload, "secret other") {
		t.Fatal("organize prompt leaked unsubscribed node statement")
	}
}

func TestCompactHistoryLeavesDuplicateJudgmentToModel(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[
		{"kind":"fact","statement":" visible   fact ","status":"accepted","subgraph_ids":["sg-a"]},
		{"kind":"fact","statement":"secret other","status":"accepted","subgraph_ids":["sg-a"]},
		{"kind":"fact","statement":"new fact","status":"accepted","subgraph_ids":["sg-a"]},
		{"kind":"fact","statement":" new   fact ","status":"accepted","subgraph_ids":["sg-a"]}
	]}`}
	graph := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-a", Kind: ctxgraph.SubgraphKindTask},
			{ID: "sg-b", Kind: ctxgraph.SubgraphKindGeneral},
		},
		Nodes: []ctxgraph.Node{
			{ID: "visible", Statement: "visible fact", SubgraphIDs: []string{"sg-a"}},
			{ID: "hidden", Statement: "secret other", SubgraphIDs: []string{"sg-b"}},
		},
	}

	gotGraph, _, err := CompactHistory(
		context.Background(),
		provider,
		graph,
		[]Message{{Role: RoleUser, Content: "old work"}},
		[]string{"sg-a"},
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)
	for _, node := range gotGraph.Nodes {
		counts[strings.Join(strings.Fields(node.Statement), " ")]++
	}
	if counts["visible fact"] != 2 {
		t.Fatalf("visible statement count = %d, want original plus model output", counts["visible fact"])
	}
	if counts["secret other"] != 2 {
		t.Fatalf("hidden statement count = %d, want hidden original plus one visible node", counts["secret other"])
	}
	if counts["new fact"] != 2 {
		t.Fatalf("new draft count = %d, want both model outputs", counts["new fact"])
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

func TestCompactHistoryClassifiesExhaustedInvalidJSON(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[`}
	_, _, err := CompactHistory(
		context.Background(),
		provider,
		ctxgraph.Graph{},
		[]Message{{Role: RoleUser, Content: "old work"}},
		nil,
		0,
		"agent-a",
	)
	if !errors.Is(err, ErrMemoryFormat) {
		t.Fatalf("error = %v, want %v", err, ErrMemoryFormat)
	}
	if provider.calls != maxOrganizeFormatAttempts {
		t.Fatalf("model calls = %d, want %d", provider.calls, maxOrganizeFormatAttempts)
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

func TestCompactHistoryDoesNotCompressMemoryToolTraffic(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[]}`}
	_, _, err := CompactHistory(
		context.Background(),
		provider,
		ctxgraph.Graph{},
		[]Message{
			{Role: RoleUser, Content: "old work"},
			{
				Role: RoleAssistant,
				ToolCalls: []agenttool.Call{
					{Name: memoryNodesInToolName, Arguments: json.RawMessage(`{"subgraph_ids":["sg-a"]}`)},
					{Name: "echo", Arguments: json.RawMessage(`{"text":"keep"}`)},
				},
			},
			{
				Role:    RoleTool,
				Content: "MEMORY_NODE_SHOULD_NOT_BE_COMPACTED",
				ToolResult: &agenttool.Result{
					Name:    memoryNodesInToolName,
					Content: "MEMORY_NODE_SHOULD_NOT_BE_COMPACTED",
				},
			},
			{
				Role:    RoleTool,
				Content: "keep result",
				ToolResult: &agenttool.Result{
					Name:    "echo",
					Content: "keep result",
				},
			},
		},
		nil,
		0,
		"agent-a",
	)
	if err != nil {
		t.Fatal(err)
	}

	payload := provider.last.Messages[0].Content
	if strings.Contains(payload, memoryNodesInToolName) || strings.Contains(payload, "MEMORY_NODE_SHOULD_NOT_BE_COMPACTED") {
		t.Fatalf("payload included memory tool traffic: %q", payload)
	}
	if !strings.Contains(payload, `echo({"text":"keep"})`) || !strings.Contains(payload, "keep result") {
		t.Fatalf("payload lost ordinary tool traffic: %q", payload)
	}
}

func TestCompactHistoryBoundsLargeOrganizerPrompt(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[]}`}
	messages := []Message{{Role: RoleUser, Content: "ORIGINAL_GOAL: preserve typed SSE requirements"}}
	for i := range 24 {
		messages = append(messages,
			Message{Role: RoleAssistant, ToolCalls: []agenttool.Call{{
				Name:      "read",
				Arguments: json.RawMessage(fmt.Sprintf(`{"path":"source-%d.ts"}`, i)),
			}}},
			Message{Role: RoleTool, Content: strings.Repeat(fmt.Sprintf("middle-%02d ", i), 4096)},
		)
	}
	messages = append(messages, Message{Role: RoleTool, Content: "RECENT_EVIDENCE: pnpm test exited 0"})

	_, tail, err := CompactHistory(
		context.Background(),
		provider,
		ctxgraph.Graph{},
		messages,
		nil,
		0,
		"planner",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 0 {
		t.Fatalf("tail = %#v, want fully compacted history", tail)
	}
	payload := provider.last.Messages[0].Content
	if len(payload) > 16<<10 {
		t.Fatalf("organizer prompt = %d bytes, want at most 16 KiB", len(payload))
	}
	for _, want := range []string{"ORIGINAL_GOAL", "RECENT_EVIDENCE", "middle omitted"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("organizer prompt lost %q", want)
		}
	}
}

func TestCompactHistoryUsesKnownContextWindowBeforeClipping(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{response: `{"nodes":[]}`}
	messages := []Message{{Role: RoleUser, Content: "ORIGINAL_GOAL"}}
	for i := range 12 {
		messages = append(messages, Message{
			Role:    RoleTool,
			Content: strings.Repeat(fmt.Sprintf("middle-%02d ", i), 1024),
		})
	}
	messages = append(messages, Message{Role: RoleTool, Content: "RECENT_EVIDENCE"})
	ctx := WithTranscript(context.Background(), Transcript{
		CacheKey:      "planner",
		CompactPrompt: "yaml compact",
		ContextWindow: 272000,
	})

	_, tail, err := CompactHistory(
		ctx,
		provider,
		ctxgraph.Graph{},
		messages,
		nil,
		0,
		"planner",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 0 {
		t.Fatalf("tail = %#v, want fully compacted history", tail)
	}
	payload := provider.last.Messages[0].Content
	if len(payload) <= maxOrganizePromptBytes {
		t.Fatalf("organizer prompt = %d bytes, want known window to exceed fallback cap", len(payload))
	}
	for _, want := range []string{"ORIGINAL_GOAL", "middle-06", "RECENT_EVIDENCE"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("organizer prompt lost %q", want)
		}
	}
	if strings.Contains(payload, "middle omitted") {
		t.Fatal("organizer prompt clipped history that fits the known context window")
	}
}
