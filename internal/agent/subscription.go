package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const memorySubscribeToolName = "memory_subscribe"

// subscriptionSink 收集 organizer 在一次 organize 查询里对请求方订阅的增删意图。
// 意图不立即生效：organize_subgraph 在整理 Ask 成功返回后统一应用，
// 这样 organizer 不能在深度整理或任何异步时刻改别人的上下文，Ask 失败则订阅零变化。
type subscriptionSink struct {
	mu          sync.Mutex
	subscribe   []string
	unsubscribe []string
}

type subscriptionSinkKey struct{}

// withSubscriptionSink 把 sink 放进 ctx；organizer 的工具经 Ask 的 ctx 链读到它。
func withSubscriptionSink(ctx context.Context, sink *subscriptionSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, subscriptionSinkKey{}, sink)
}

func subscriptionSinkFrom(ctx context.Context) *subscriptionSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(subscriptionSinkKey{}).(*subscriptionSink)
	return sink
}

func (s *subscriptionSink) record(subscribe, unsubscribe []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribe = uniqueIDs(append(s.subscribe, subscribe...))
	s.unsubscribe = uniqueIDs(append(s.unsubscribe, unsubscribe...))
}

// pending 返回本次查询累积的订阅增删意图；同一 ID 多次出现只算一次。
func (s *subscriptionSink) pending() (subscribe, unsubscribe []string) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.subscribe...), append([]string(nil), s.unsubscribe...)
}

// MemorySubscribeTool 让整理 Agent 在服务一次 organize 查询时调整请求方的动态订阅。
// 只记录意图，由 organize_subgraph 在查询收尾时应用。
func MemorySubscribeTool() agenttool.Tool { return memorySubscribeTool{} }

type memorySubscribeTool struct{}

var _ agenttool.Tool = memorySubscribeTool{}

type memorySubscribeArgs struct {
	Subscribe   []string `json:"subscribe"`
	Unsubscribe []string `json:"unsubscribe"`
	Reason      string   `json:"reason"`
}

type memorySubscribeResult struct {
	Recorded    string   `json:"recorded"`
	Subscribe   []string `json:"subscribe,omitempty"`
	Unsubscribe []string `json:"unsubscribe,omitempty"`
}

func (memorySubscribeTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        memorySubscribeToolName,
		Description: "调整发起本次 organize 查询的 Agent 的动态子图订阅：subscribe 让它开始看到某些子图，unsubscribe 让它不再看到。只在服务 organize 查询时可用，且在该查询完成后才生效；每次调用必须给 reason。取消订阅只作用于动态订阅，任务启动包与运行时固定订阅不可取消。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"subscribe":{"type":"array","items":{"type":"string"},"description":"要给请求方新增订阅的子图 ID"},"unsubscribe":{"type":"array","items":{"type":"string"},"description":"要从请求方动态订阅里去掉的子图 ID"},"reason":{"type":"string","description":"调整理由，必填；取消订阅须引用请求方表达的“不需要”或明确的失效证据"}},"required":["reason"],"additionalProperties":false}`),
	}
}

func (t memorySubscribeTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	raw := call.Arguments
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var args memorySubscribeArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agenttool.Output{}, fmt.Errorf("%s: decode arguments: %w", memorySubscribeToolName, err)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return agenttool.Output{}, fmt.Errorf("%s: reason is required", memorySubscribeToolName)
	}
	subscribe := uniqueIDs(trimIDs(args.Subscribe))
	unsubscribe := uniqueIDs(trimIDs(args.Unsubscribe))
	if len(subscribe) == 0 && len(unsubscribe) == 0 {
		return agenttool.Output{}, fmt.Errorf("%s: subscribe or unsubscribe must list at least one subgraph", memorySubscribeToolName)
	}
	for _, id := range subscribe {
		if containsString(unsubscribe, id) {
			return agenttool.Output{}, fmt.Errorf("%s: subgraph %q is in both subscribe and unsubscribe", memorySubscribeToolName, id)
		}
	}

	sink := subscriptionSinkFrom(ctx)
	if sink == nil {
		return agenttool.Output{}, fmt.Errorf(
			"%s: only available while serving an organize query; deep curation cannot change anyone's subscriptions",
			memorySubscribeToolName,
		)
	}
	sink.record(subscribe, unsubscribe)

	return marshalToolJSON(memorySubscribeResult{
		Recorded:    "已记录，本次查询完成后生效",
		Subscribe:   subscribe,
		Unsubscribe: unsubscribe,
	})
}

func trimIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func marshalToolJSON(value any) (agenttool.Output, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return agenttool.Output{}, err
	}
	return agenttool.Output{Content: string(payload)}, nil
}
