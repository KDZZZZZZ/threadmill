// Package event 把运行时生命周期归一成 RuntimeEvent，再分发给监控等消费者。
//
// 生产者可以是业务代码（ReAct Loop）或日后的 Eino OnStart/OnEnd/OnError 回调。
// 实时落库与 WebSocket 不在本包。
package event

import (
	"context"
	"time"
)

// Kind 是触发实体的种类，对应 Eino RunInfo.Component 的 ChatModel / Tool。
type Kind string

const (
	KindModel  Kind = "model"
	KindTool   Kind = "tool"
	KindTask   Kind = "task"
	KindMemory Kind = "memory"
)

// Phase 是一次调用的开始、结束、流活动或重试时点；失败写在 End 的 Err 上。
type Phase string

const (
	PhaseStart Phase = "start"
	PhaseEnd   Phase = "end"
	PhaseDelta Phase = "delta"
	PhaseRetry Phase = "retry"
)

// RuntimeEvent 是模型/工具调用的归一化记录。
type RuntimeEvent struct {
	Time             time.Time     `json:"time"`
	AgentID          string        `json:"agent_id,omitempty"`
	Kind             Kind          `json:"kind"`
	Phase            Phase         `json:"phase"`
	Name             string        `json:"name,omitempty"`
	CallID           string        `json:"call_id,omitempty"`
	Duration         time.Duration `json:"duration,omitempty"`
	Err              string        `json:"error,omitempty"`
	IsError          bool          `json:"is_error,omitempty"`
	Messages         int           `json:"messages,omitempty"`
	Tools            int           `json:"tools,omitempty"`
	ToolCalls        int           `json:"tool_calls,omitempty"`
	Tokens           int           `json:"tokens,omitempty"`
	CachedTokens     int           `json:"cached_tokens,omitempty"`
	Retries          int           `json:"retries,omitempty"`
	RetryReason      string        `json:"retry_reason,omitempty"`
	Delta            string        `json:"delta,omitempty"`
	StreamText       bool          `json:"stream_text,omitempty"`
	MemoryOrganized  bool          `json:"memory_organized,omitempty"`
	MemoryCandidates int           `json:"memory_candidates,omitempty"`
	MemorySelected   int           `json:"memory_selected,omitempty"`
}

// Input 是生产者交给 Normalize 的原始字段。
type Input struct {
	AgentID      string
	Kind         Kind
	Name         string
	CallID       string
	Started      time.Time
	Messages     int
	Tools        int
	ToolCalls    int
	Tokens       int
	CachedTokens int
	IsError      bool
	Err          error
}

// Normalize 把一次生产者回调收成 RuntimeEvent。
func Normalize(in Input, phase Phase) RuntimeEvent {
	now := time.Now()
	ev := RuntimeEvent{
		Time:         now,
		AgentID:      in.AgentID,
		Kind:         in.Kind,
		Phase:        phase,
		Name:         in.Name,
		CallID:       in.CallID,
		IsError:      in.IsError,
		Messages:     in.Messages,
		Tools:        in.Tools,
		ToolCalls:    in.ToolCalls,
		Tokens:       in.Tokens,
		CachedTokens: in.CachedTokens,
	}
	if phase == PhaseEnd && !in.Started.IsZero() {
		ev.Duration = now.Sub(in.Started)
	}
	if in.Err != nil {
		ev.Err = in.Err.Error()
		ev.IsError = true
	}
	return ev
}

// ModelStart 归一化一次模型调用开始。
func ModelStart(agentID string, messages, tools int) RuntimeEvent {
	return Normalize(Input{
		AgentID:  agentID,
		Kind:     KindModel,
		Messages: messages,
		Tools:    tools,
	}, PhaseStart)
}

// ModelEnd 归一化一次模型调用结束；cachedTokens 是本次命中前缀缓存的输入 token 数。
func ModelEnd(agentID, name string, started time.Time, toolCalls, tokens, cachedTokens int, err error) RuntimeEvent {
	return Normalize(Input{
		AgentID:      agentID,
		Kind:         KindModel,
		Name:         name,
		Started:      started,
		ToolCalls:    toolCalls,
		Tokens:       tokens,
		CachedTokens: cachedTokens,
		Err:          err,
	}, PhaseEnd)
}

// ModelRetry 记录一次即将重放的模型请求；attempt 从 1 开始。
func ModelRetry(agentID string, attempt int, reason string) RuntimeEvent {
	return RuntimeEvent{
		Time:        time.Now(),
		AgentID:     agentID,
		Kind:        KindModel,
		Phase:       PhaseRetry,
		Retries:     attempt,
		RetryReason: reason,
	}
}

// ToolStart 归一化一次工具调用开始。
func ToolStart(agentID, name, callID string) RuntimeEvent {
	return Normalize(Input{
		AgentID: agentID,
		Kind:    KindTool,
		Name:    name,
		CallID:  callID,
	}, PhaseStart)
}

// ToolEnd 归一化一次工具调用结束。
func ToolEnd(agentID, name, callID string, started time.Time, isError bool, err error) RuntimeEvent {
	return Normalize(Input{
		AgentID: agentID,
		Kind:    KindTool,
		Name:    name,
		CallID:  callID,
		Started: started,
		IsError: isError,
		Err:     err,
	}, PhaseEnd)
}

// TaskStart 归一化一次协调任务开始。
func TaskStart(taskID string) RuntimeEvent {
	return Normalize(Input{AgentID: taskID, Kind: KindTask}, PhaseStart)
}

// TaskEnd 归一化一次协调任务结束；name 是 done/failed/canceled 终态。
func TaskEnd(taskID, name string, started time.Time, err error) RuntimeEvent {
	return Normalize(Input{
		AgentID: taskID,
		Kind:    KindTask,
		Name:    name,
		Started: started,
		Err:     err,
	}, PhaseEnd)
}

// MemoryStart 归一化一次隐藏记忆操作开始。
func MemoryStart(agentID, name, callID string) RuntimeEvent {
	return Normalize(Input{
		AgentID: agentID,
		Kind:    KindMemory,
		Name:    name,
		CallID:  callID,
	}, PhaseStart)
}

// MemoryEnd 归一化一次隐藏记忆操作结束。
func MemoryEnd(agentID, name, callID string, started time.Time, err error) RuntimeEvent {
	return Normalize(Input{
		AgentID: agentID,
		Kind:    KindMemory,
		Name:    name,
		CallID:  callID,
		Started: started,
		Err:     err,
	}, PhaseEnd)
}

// MemoryDelta records provider stream activity inside a hidden memory operation.
// It intentionally carries no response text.
func MemoryDelta(agentID, name, callID string, text bool) RuntimeEvent {
	return RuntimeEvent{
		Time:       time.Now(),
		AgentID:    agentID,
		Kind:       KindMemory,
		Phase:      PhaseDelta,
		Name:       name,
		CallID:     callID,
		StreamText: text,
	}
}

// MemoryOrganized records the candidate and selected node counts for one
// organizer pass. A zero selection remains observable through MemoryOrganized.
func MemoryOrganized(
	agentID, name, callID string,
	started time.Time,
	candidates, selected int,
	err error,
) RuntimeEvent {
	ev := MemoryEnd(agentID, name, callID, started, err)
	ev.MemoryOrganized = true
	ev.MemoryCandidates = candidates
	ev.MemorySelected = selected
	return ev
}

// ModelDelta 归一化一段流式文本增量。
func ModelDelta(agentID, delta string) RuntimeEvent {
	return RuntimeEvent{
		Time:    time.Now(),
		AgentID: agentID,
		Kind:    KindModel,
		Phase:   PhaseDelta,
		Delta:   delta,
	}
}

type deltaKey struct{}

type retryKey struct{}

type replayableDeltasKey struct{}

type deltaActivityKey struct{}

// WithDeltaSink 把文本增量回调挂到 ctx 上，供 Provider 在流式生成时调用。
func WithDeltaSink(ctx context.Context, sink func(string)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deltaKey{}, sink)
}

// DeltaSink 取出 ctx 上的增量回调；没有时返回 nil。
func DeltaSink(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(deltaKey{}).(func(string))
	return sink
}

// WithReplayableDeltas 表示增量尚未直接交付给用户，Provider 可在流中断时安全重放。
func WithReplayableDeltas(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, replayableDeltasKey{}, true)
}

// ReplayableDeltas 报告 Provider 是否可以丢弃一次失败尝试的增量并重试。
func ReplayableDeltas(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	replayable, _ := ctx.Value(replayableDeltasKey{}).(bool)
	return replayable
}

// WithDeltaActivitySink 把不含正文的流活动回调挂到 ctx 上，供监控记录流式进度。
func WithDeltaActivitySink(ctx context.Context, sink func(text bool)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, deltaActivityKey{}, sink)
}

// DeltaActivitySink 取出流活动回调；没有时返回 nil。
func DeltaActivitySink(ctx context.Context) func(bool) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(deltaActivityKey{}).(func(bool))
	return sink
}

// WithRetrySink 把 Provider 重试回调挂到 ctx 上，供模型事件聚合重试次数。
func WithRetrySink(ctx context.Context, sink func(string)) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, retryKey{}, sink)
}

// RetrySink 取出 ctx 上的 Provider 重试回调；没有时返回 nil。
func RetrySink(ctx context.Context) func(string) {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(retryKey{}).(func(string))
	return sink
}
