// Package event 把模型调用和工具调用归一成 RuntimeEvent，再分发给监控等消费者。
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
	KindModel Kind = "model"
	KindTool  Kind = "tool"
)

// Phase 是一次调用的时点，对应 Eino Timing OnStart / OnEnd；失败写在 End 的 Err 上。
type Phase string

const (
	PhaseStart Phase = "start"
	PhaseEnd   Phase = "end"
	PhaseDelta Phase = "delta"
)

// RuntimeEvent 是模型/工具调用的归一化记录。
type RuntimeEvent struct {
	Time      time.Time     `json:"time"`
	AgentID   string        `json:"agent_id,omitempty"`
	Kind      Kind          `json:"kind"`
	Phase     Phase         `json:"phase"`
	Name      string        `json:"name,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	Err       string        `json:"error,omitempty"`
	IsError   bool          `json:"is_error,omitempty"`
	Messages  int           `json:"messages,omitempty"`
	Tools     int           `json:"tools,omitempty"`
	ToolCalls int           `json:"tool_calls,omitempty"`
	Tokens    int           `json:"tokens,omitempty"`
	Delta     string        `json:"delta,omitempty"`
}

// Input 是生产者交给 Normalize 的原始字段。
type Input struct {
	AgentID   string
	Kind      Kind
	Name      string
	CallID    string
	Started   time.Time
	Messages  int
	Tools     int
	ToolCalls int
	Tokens    int
	IsError   bool
	Err       error
}

// Normalize 把一次生产者回调收成 RuntimeEvent。
func Normalize(in Input, phase Phase) RuntimeEvent {
	now := time.Now()
	ev := RuntimeEvent{
		Time:      now,
		AgentID:   in.AgentID,
		Kind:      in.Kind,
		Phase:     phase,
		Name:      in.Name,
		CallID:    in.CallID,
		IsError:   in.IsError,
		Messages:  in.Messages,
		Tools:     in.Tools,
		ToolCalls: in.ToolCalls,
		Tokens:    in.Tokens,
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

// ModelEnd 归一化一次模型调用结束。
func ModelEnd(agentID, name string, started time.Time, toolCalls, tokens int, err error) RuntimeEvent {
	return Normalize(Input{
		AgentID:   agentID,
		Kind:      KindModel,
		Name:      name,
		Started:   started,
		ToolCalls: toolCalls,
		Tokens:    tokens,
		Err:       err,
	}, PhaseEnd)
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
