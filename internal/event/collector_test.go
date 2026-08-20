package event

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCollectorAggregatesLifecycleLatencyAndTTFT(t *testing.T) {
	t.Parallel()

	collector := NewCollector()
	base := time.Unix(100, 0)
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base, AgentID: "planner", Kind: KindModel, Phase: PhaseStart,
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(25 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta, Delta: "你",
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(30 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta, Delta: "好",
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(50 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseEnd,
		Duration: 50 * time.Millisecond, Tokens: 42, ToolCalls: 1,
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base, AgentID: "executor", Kind: KindTool, Phase: PhaseStart, Name: "bash", CallID: "call-1",
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(10 * time.Millisecond), AgentID: "executor", Kind: KindTool, Phase: PhaseEnd,
		Name: "bash", CallID: "call-1", Duration: 10 * time.Millisecond, IsError: true, Err: "exit 1",
	})

	got := collector.Snapshot()
	if got.Model.Started != 1 || got.Model.Completed != 1 || got.Model.Active != 0 || got.Model.Errors != 0 {
		t.Fatalf("model = %#v", got.Model)
	}
	if got.Model.Duration.Count != 1 || got.Model.Duration.Total != 50*time.Millisecond || got.Model.Duration.Max != 50*time.Millisecond {
		t.Fatalf("model duration = %#v", got.Model.Duration)
	}
	if got.Model.TTFT.Count != 1 || got.Model.TTFT.Total != 25*time.Millisecond || got.Model.TTFT.Max != 25*time.Millisecond {
		t.Fatalf("model TTFT = %#v", got.Model.TTFT)
	}
	if got.Tool.Started != 1 || got.Tool.Completed != 1 || got.Tool.Errors != 1 || got.Tool.Active != 0 {
		t.Fatalf("tool = %#v", got.Tool)
	}
	if got.Tokens != 42 || got.ToolCalls != 1 || got.DeltaChunks != 2 || got.DeltaBytes != 6 {
		t.Fatalf("totals = %#v", got)
	}
}

func TestCollectorTracksTasksAndNeverMakesActiveNegative(t *testing.T) {
	t.Parallel()

	collector := NewCollector()
	collector.Handle(context.Background(), TaskStart("task-1"))
	active := collector.Snapshot()
	if active.Task.Started != 1 || active.Task.Active != 1 {
		t.Fatalf("active task = %#v", active.Task)
	}
	collector.Handle(context.Background(), TaskEnd(
		"task-1", "failed", time.Now().Add(-time.Second), errors.New("boom"),
	))
	collector.Handle(context.Background(), RuntimeEvent{Kind: KindTask, Phase: PhaseEnd})
	got := collector.Snapshot()
	if got.Task.Completed != 2 || got.Task.Errors != 1 || got.Task.Active != 0 {
		t.Fatalf("task = %#v", got.Task)
	}
	collector.Handle(context.Background(), MemoryStart("manager", "compact_memory", "hidden-compact_memory"))
	collector.Handle(context.Background(), MemoryEnd(
		"manager", "compact_memory", "hidden-compact_memory", time.Now().Add(-time.Millisecond), nil,
	))
	if memory := collector.Snapshot().Memory; memory.Started != 1 || memory.Completed != 1 || memory.Errors != 0 {
		t.Fatalf("memory = %#v", memory)
	}
}

func TestCollectorUsesBoundedCumulativeHistograms(t *testing.T) {
	t.Parallel()

	collector := NewCollector()
	collector.Handle(context.Background(), RuntimeEvent{
		Kind: KindTool, Phase: PhaseEnd, Duration: 75 * time.Millisecond,
	})
	got := collector.Snapshot().Tool.Duration
	if len(got.Buckets) == 0 {
		t.Fatal("duration buckets are empty")
	}
	var previous uint64
	for _, bucket := range got.Buckets {
		if bucket.Count < previous {
			t.Fatalf("buckets are not cumulative: %#v", got.Buckets)
		}
		previous = bucket.Count
	}
	if got.Buckets[len(got.Buckets)-1].Count != got.Count {
		t.Fatalf("last bucket = %d, count = %d", got.Buckets[len(got.Buckets)-1].Count, got.Count)
	}
}
