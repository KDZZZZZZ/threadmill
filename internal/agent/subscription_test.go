package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// organizerCalling 让整理 Agent 在第一轮发出给定的工具调用，第二轮直接收尾。
func organizerCalling(calls ...agenttool.Call) modelFunc {
	step := 0
	return ignoreOrganize(func(context.Context, Request) (AssistantMessage, error) {
		step++
		if step == 1 {
			return AssistantMessage{ToolCalls: calls}, nil
		}
		return AssistantMessage{Content: "ok"}, nil
	})
}

// subscriptionFixture 组一个请求方 + 整理 Agent，共用同一份记忆图。
type subscriptionFixture struct {
	requester *Loop
	tool      agenttool.Tool
	store     *ctxgraph.Store
}

func newSubscriptionFixture(t *testing.T, organizerModel modelFunc) subscriptionFixture {
	t.Helper()
	resetDefaultStore(t)

	organizer, err := NewSubgraphOrganizer(Config{Provider: organizerModel})
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
		Subgraphs: []ctxgraph.Subgraph{
			{ID: "sg-keep", Kind: ctxgraph.SubgraphKindGeneral},
			{ID: "sg-drop", Kind: ctxgraph.SubgraphKindGeneral},
			{ID: "sg-offer", Kind: ctxgraph.SubgraphKindGeneral},
		},
	})
	tool, ok := requester.tools[organizeSubgraphToolName]
	if !ok {
		t.Fatalf("%s not registered on requester", organizeSubgraphToolName)
	}
	return subscriptionFixture{requester: requester, tool: tool, store: store}
}

func subscribeCall(args string) agenttool.Call {
	return agenttool.Call{ID: "sub-1", Name: memorySubscribeToolName, Arguments: json.RawMessage(args)}
}

func runOrganize(t *testing.T, f subscriptionFixture, args string) organizeSubgraphResult {
	t.Helper()
	out, err := f.tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      organizeSubgraphToolName,
		Arguments: json.RawMessage(args),
	})
	if err != nil {
		t.Fatalf("%s Execute() error = %v", organizeSubgraphToolName, err)
	}
	var result organizeSubgraphResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatalf("decode result %q: %v", out.Content, err)
	}
	return result
}

func TestOrganizeSubgraphAppliesOrganizerSubscriptionChanges(t *testing.T) {
	f := newSubscriptionFixture(t, organizerCalling(subscribeCall(
		`{"subscribe":["sg-offer"],"unsubscribe":["sg-drop"],"reason":"请求方声明不再需要 sg-drop"}`,
	)))
	f.requester.SetSubscribedSubgraphs([]string{"sg-keep", "sg-drop"})

	result := runOrganize(t, f, `{"query":"blue","exclude":"sg-drop 的内容"}`)

	if got, want := result.Subscriptions.Subscribed, []string{"sg-offer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscribed = %v, want %v", got, want)
	}
	if got, want := result.Subscriptions.Unsubscribed, []string{"sg-drop"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unsubscribed = %v, want %v", got, want)
	}
	if len(result.Subscriptions.Skipped) != 0 {
		t.Fatalf("skipped = %#v, want none", result.Subscriptions.Skipped)
	}
	want := []string{"sg-keep", "sg-offer", result.Subgraph.ID}
	if got := f.requester.subscribedSubgraphs; !reflect.DeepEqual(got, want) {
		t.Fatalf("requester subscriptions = %v, want %v", got, want)
	}
}

func TestOrganizeSubgraphReportsSkippedSubscriptionChanges(t *testing.T) {
	f := newSubscriptionFixture(t, organizerCalling(subscribeCall(
		`{"subscribe":["sg-missing"],"unsubscribe":["sg-package"],"reason":"试探边界"}`,
	)))
	f.requester.SetStableSubscribedSubgraphs([]string{"sg-package"})
	f.requester.SetSubscribedSubgraphs([]string{"sg-keep"})

	result := runOrganize(t, f, `{"query":"blue"}`)

	if len(result.Subscriptions.Subscribed) != 0 || len(result.Subscriptions.Unsubscribed) != 0 {
		t.Fatalf("subscriptions = %#v, want no applied changes", result.Subscriptions)
	}
	reasons := map[string]string{}
	for _, skip := range result.Subscriptions.Skipped {
		reasons[skip.ID] = skip.Reason
	}
	if !strings.Contains(reasons["sg-missing"], "does not exist") {
		t.Fatalf("sg-missing skip reason = %q", reasons["sg-missing"])
	}
	if !strings.Contains(reasons["sg-package"], "not a dynamic subscription") {
		t.Fatalf("sg-package skip reason = %q", reasons["sg-package"])
	}
	if got, want := f.requester.subscribedSubgraphs, []string{"sg-keep", result.Subgraph.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requester subscriptions = %v, want %v", got, want)
	}
	if got := f.requester.stableSubscribedSubgraphs; !reflect.DeepEqual(got, []string{"sg-package"}) {
		t.Fatalf("stable subscriptions = %v, want the task package intact", got)
	}
}

func TestOrganizeSubgraphKeepsTargetSubgraphSubscribed(t *testing.T) {
	var target string
	f := newSubscriptionFixture(t, ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		if target == "" {
			// 目标子图 ID 只在整理请求里出现，取出来再试着取消订阅它。
			for _, field := range strings.Fields(request.Messages[0].Content) {
				if strings.HasPrefix(field, querySubgraphPrefix) {
					target = field
				}
			}
			return AssistantMessage{ToolCalls: []agenttool.Call{subscribeCall(
				`{"unsubscribe":["` + target + `"],"reason":"试着丢掉本次结果"}`,
			)}}, nil
		}
		return AssistantMessage{Content: "ok"}, nil
	}))

	result := runOrganize(t, f, `{"query":"blue"}`)

	if target == "" || target != result.Subgraph.ID {
		t.Fatalf("target = %q, subgraph = %q", target, result.Subgraph.ID)
	}
	if len(result.Subscriptions.Unsubscribed) != 0 {
		t.Fatalf("unsubscribed = %v, want the query target to stay", result.Subscriptions.Unsubscribed)
	}
	if len(result.Subscriptions.Skipped) != 1 ||
		!strings.Contains(result.Subscriptions.Skipped[0].Reason, "stays subscribed") {
		t.Fatalf("skipped = %#v", result.Subscriptions.Skipped)
	}
	if got := f.requester.subscribedSubgraphs; !reflect.DeepEqual(got, []string{result.Subgraph.ID}) {
		t.Fatalf("requester subscriptions = %v, want the organized subgraph", got)
	}
}

func TestOrganizeSubgraphLeavesSubscriptionsUnchangedWhenOrganizeFails(t *testing.T) {
	step := 0
	f := newSubscriptionFixture(t, ignoreOrganize(func(context.Context, Request) (AssistantMessage, error) {
		step++
		if step == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{subscribeCall(
				`{"unsubscribe":["sg-drop"],"reason":"请求方声明不再需要"}`,
			)}}, nil
		}
		return AssistantMessage{}, errors.New("organizer exploded")
	}))
	f.requester.SetSubscribedSubgraphs([]string{"sg-keep", "sg-drop"})

	_, err := f.tool.Execute(context.Background(), agenttool.Call{
		ID:        "call-1",
		Name:      organizeSubgraphToolName,
		Arguments: json.RawMessage(`{"query":"blue"}`),
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want the organizer failure")
	}
	want := []string{"sg-keep", "sg-drop"}
	if got := f.requester.subscribedSubgraphs; !reflect.DeepEqual(got, want) {
		t.Fatalf("subscriptions = %v, want %v unchanged after a failed organize", got, want)
	}
}

func TestOrganizeSubgraphPassesExcludeToOrganizer(t *testing.T) {
	var prompt string
	f := newSubscriptionFixture(t, ignoreOrganize(func(_ context.Context, request Request) (AssistantMessage, error) {
		if prompt == "" && len(request.Messages) > 0 {
			prompt = request.Messages[0].Content
		}
		return AssistantMessage{Content: "ok"}, nil
	}))

	runOrganize(t, f, `{"query":"验收命令","exclude":"早期实现讨论"}`)

	if !strings.Contains(prompt, "验收命令") {
		t.Fatalf("organize request = %q, want the query", prompt)
	}
	if !strings.Contains(prompt, "不需要") || !strings.Contains(prompt, "早期实现讨论") {
		t.Fatalf("organize request = %q, want the exclude clause", prompt)
	}
}

func TestOrganizeSubgraphCatalogCarriesAdmissionAndScope(t *testing.T) {
	got := organizeQuery("查询", "", "sg-q-9", ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{
			ID:        "sg-seed",
			Kind:      ctxgraph.SubgraphKindGeneral,
			Name:      "验收契约",
			Summary:   "回答 X 怎样算通过",
			Admission: "只收带命令与退出码的 fact",
			Scope:     "verifier 验收阶段",
		}},
	}, "")
	for _, want := range []string{"回答 X 怎样算通过", "只收带命令与退出码的 fact", "verifier 验收阶段"} {
		if !strings.Contains(got, want) {
			t.Fatalf("organizeQuery() = %q, want subgraph description %q", got, want)
		}
	}
}

func TestNextQuerySubgraphIDSkipsPersistedIDs(t *testing.T) {
	graph := ctxgraph.Graph{Subgraphs: []ctxgraph.Subgraph{
		{ID: "sg-q-1"},
		{ID: "sg-q-7"},
		{ID: "sg-q-not-a-number"},
		{ID: "sg-seed"},
	}}
	if got, want := nextQuerySubgraphID(graph), "sg-q-8"; got != want {
		t.Fatalf("nextQuerySubgraphID() = %q, want %q", got, want)
	}
	if got, want := nextQuerySubgraphID(ctxgraph.Graph{}), "sg-q-1"; got != want {
		t.Fatalf("nextQuerySubgraphID(empty) = %q, want %q", got, want)
	}
}

func TestMemorySubscribeRequiresOrganizeQueryContext(t *testing.T) {
	tool := MemorySubscribeTool()
	_, err := tool.Execute(context.Background(), subscribeCall(
		`{"unsubscribe":["sg-x"],"reason":"深度整理里顺手改"}`,
	))
	if err == nil || !strings.Contains(err.Error(), "organize query") {
		t.Fatalf("Execute() error = %v, want the missing-sink refusal", err)
	}
}

func TestMemorySubscribeRejectsMalformedRequests(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"no reason", `{"subscribe":["sg-a"]}`, "reason is required"},
		{"nothing to do", `{"reason":"r"}`, "at least one subgraph"},
		{"contradiction", `{"subscribe":["sg-a"],"unsubscribe":["sg-a"],"reason":"r"}`, "both subscribe and unsubscribe"},
	}
	sink := &subscriptionSink{}
	ctx := withSubscriptionSink(context.Background(), sink)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MemorySubscribeTool().Execute(ctx, subscribeCall(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute() error = %v, want %q", err, tc.want)
			}
		})
	}
	if subscribe, unsubscribe := sink.pending(); len(subscribe) != 0 || len(unsubscribe) != 0 {
		t.Fatalf("rejected calls reached the sink: %v %v", subscribe, unsubscribe)
	}
}

func TestMemorySubscribeIsOrganizerOnly(t *testing.T) {
	if _, ok := organizerOnlyTools[memorySubscribeToolName]; !ok {
		t.Fatalf("%s must stay organizer-only", memorySubscribeToolName)
	}
	if err := MemorySubscribeTool().Definition().Validate(); err != nil {
		t.Fatalf("Definition().Validate() = %v", err)
	}
}
