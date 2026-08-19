package event

import (
	"context"
	"log/slog"
)

// Monitor 把 RuntimeEvent 写成结构化日志，作为监控消费者。
func Monitor(logger *slog.Logger) Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, ev RuntimeEvent) {
		level := slog.LevelInfo
		if ev.Err != "" {
			level = slog.LevelError
		}
		attrs := []any{
			"kind", ev.Kind,
			"phase", ev.Phase,
		}
		if ev.AgentID != "" {
			attrs = append(attrs, "agent_id", ev.AgentID)
		}
		if ev.Name != "" {
			attrs = append(attrs, "name", ev.Name)
		}
		if ev.CallID != "" {
			attrs = append(attrs, "call_id", ev.CallID)
		}
		if ev.Duration > 0 {
			attrs = append(attrs, "duration", ev.Duration)
		}
		if ev.Err != "" {
			attrs = append(attrs, "error", ev.Err)
		}
		if ev.IsError {
			attrs = append(attrs, "is_error", true)
		}
		if ev.Messages > 0 {
			attrs = append(attrs, "messages", ev.Messages)
		}
		if ev.Tools > 0 {
			attrs = append(attrs, "tools", ev.Tools)
		}
		if ev.ToolCalls > 0 {
			attrs = append(attrs, "tool_calls", ev.ToolCalls)
		}
		if ev.Tokens > 0 {
			attrs = append(attrs, "tokens", ev.Tokens)
		}
		logger.Log(ctx, level, "runtime event", attrs...)
	}
}
