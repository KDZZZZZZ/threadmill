package event

import (
	"context"
	"testing"
)

func TestBusPublishFansOut(t *testing.T) {
	var first, second []RuntimeEvent
	bus := NewBus(
		func(_ context.Context, ev RuntimeEvent) { first = append(first, ev) },
		func(_ context.Context, ev RuntimeEvent) { second = append(second, ev) },
	)

	ev := ModelStart("manager", 1, 0)
	bus.Publish(context.Background(), ev)

	if len(first) != 1 || first[0].Kind != KindModel {
		t.Fatalf("first = %#v", first)
	}
	if len(second) != 1 || second[0].AgentID != "manager" {
		t.Fatalf("second = %#v", second)
	}
}

func TestBusPublishNilIsNoop(t *testing.T) {
	var bus *Bus
	bus.Publish(context.Background(), ModelStart("x", 0, 0))
}

func TestBusSkipsNilHandler(t *testing.T) {
	calls := 0
	bus := NewBus(nil, func(context.Context, RuntimeEvent) { calls++ })
	bus.Publish(context.Background(), ToolStart("a", "read", "c1"))
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
