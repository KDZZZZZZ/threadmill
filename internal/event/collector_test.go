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
		Time: base.Add(25 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta,
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(25 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta, Delta: "你",
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(30 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta,
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(30 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta, Delta: "好",
	})
	collector.Handle(context.Background(), ModelRetry("planner", 1, "transport"))
	collector.Handle(context.Background(), ModelRetry("planner", 2, "stream_server_error"))
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(50 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseEnd,
		Duration: 50 * time.Millisecond, Tokens: 42, ToolCalls: 1, Retries: 2,
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
	if got.Tokens != 42 || got.ToolCalls != 1 || got.ModelRetries != 2 || got.StreamChunks != 2 || got.DeltaChunks != 2 || got.DeltaBytes != 6 {
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
	memoryEnd := MemoryEnd(
		"manager", "compact_memory", "hidden-compact_memory", time.Now().Add(-time.Millisecond), nil,
	)
	memoryEnd.Tokens = 17
	memoryEnd.Retries = 2
	collector.Handle(context.Background(), memoryEnd)
	snapshot := collector.Snapshot()
	if memory := snapshot.Memory; memory.Started != 1 || memory.Completed != 1 || memory.Errors != 0 {
		t.Fatalf("memory = %#v", memory)
	}
	if snapshot.MemoryTokens != 17 || snapshot.MemoryRetries != 2 {
		t.Fatalf("memory cost = %#v", snapshot)
	}
	collector.Handle(context.Background(), MemoryStart(
		"task-1:custom-organizer", "organize_subgraph", "sg-q-1",
	))
	if idle := collector.Snapshot().MemoryStreamIdle; idle != 0 {
		t.Fatalf("organizer counted as hidden memory stream idle: %s", idle)
	}
	collector.Handle(context.Background(), ModelEnd(
		"task-1:custom-organizer", "model", time.Now().Add(-time.Millisecond), 0, 7, 0, nil,
	))
	collector.Handle(context.Background(), MemoryOrganized(
		"task-1:custom-organizer",
		"organize_subgraph",
		"sg-q-1",
		time.Now().Add(-time.Millisecond),
		12,
		3,
		nil,
	))
	snapshot = collector.Snapshot()
	if snapshot.MemoryOrganizerRuns != 1 ||
		snapshot.MemoryOrganizerCandidates != 12 ||
		snapshot.MemoryOrganizerSelected != 3 ||
		snapshot.MemoryOrganizerTokens != 7 ||
		snapshot.MemoryOrganizerDuration.Count != 1 {
		t.Fatalf("memory organizer = %#v", snapshot)
	}
}

func TestCollectorTracksMemoryStreamActivity(t *testing.T) {
	t.Parallel()

	collector := NewCollector()
	base := time.Now().Add(-time.Second)
	start := MemoryStart("manager", "compact_memory", "hidden-compact_memory")
	start.Time = base
	collector.Handle(context.Background(), start)
	activity := MemoryDelta("manager", "compact_memory", "hidden-compact_memory", false)
	activity.Time = base.Add(20 * time.Millisecond)
	collector.Handle(context.Background(), activity)
	activity = MemoryDelta("manager", "compact_memory", "hidden-compact_memory", true)
	activity.Time = base.Add(30 * time.Millisecond)
	collector.Handle(context.Background(), activity)

	active := collector.Snapshot()
	if active.MemoryStreamChunks != 2 || active.MemoryStreamIdle <= 0 {
		t.Fatalf("active memory stream = %#v", active)
	}
	if active.Memory.TTFT.Count != 1 || active.Memory.TTFT.Total != 30*time.Millisecond {
		t.Fatalf("memory TTFT = %#v", active.Memory.TTFT)
	}

	end := MemoryEnd("manager", "compact_memory", "hidden-compact_memory", base, nil)
	collector.Handle(context.Background(), end)
	if got := collector.Snapshot().MemoryStreamIdle; got != 0 {
		t.Fatalf("completed memory stream idle = %s, want 0", got)
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
	if got.P50 > got.Max || got.P95 > got.Max {
		t.Fatalf("quantiles exceed observed max: %#v", got)
	}
}

func TestCollectorUsesEmptyDeltaAsStreamActivity(t *testing.T) {
	t.Parallel()

	base := time.Now()
	collector := NewCollector()
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base, AgentID: "planner", Kind: KindModel, Phase: PhaseStart,
	})
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(25 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta,
	})
	got := collector.Snapshot()
	if got.Model.TTFT.Count != 0 {
		t.Fatalf("activity recorded as text TTFT: %#v", got.Model.TTFT)
	}
	if got.DeltaChunks != 0 || got.DeltaBytes != 0 {
		t.Fatalf("delta totals = %d/%d, want timing only", got.DeltaChunks, got.DeltaBytes)
	}
	if got.StreamChunks != 1 || got.ModelStreamIdle < 0 {
		t.Fatalf("stream activity = %#v", got)
	}
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(30 * time.Millisecond), AgentID: "planner", Kind: KindModel,
		Phase: PhaseDelta, StreamText: true,
	})
	got = collector.Snapshot()
	if got.Model.TTFT.Count != 1 || got.Model.TTFT.Max != 30*time.Millisecond {
		t.Fatalf("text activity TTFT = %#v", got.Model.TTFT)
	}
	collector.Handle(context.Background(), RuntimeEvent{
		Time: base.Add(40 * time.Millisecond), AgentID: "planner", Kind: KindModel, Phase: PhaseDelta, Delta: "x",
	})
	got = collector.Snapshot()
	if got.Model.TTFT.Count != 1 || got.Model.TTFT.Max != 30*time.Millisecond {
		t.Fatalf("text TTFT = %#v", got.Model.TTFT)
	}
}

func TestCollectorReportsActiveModelStreamIdle(t *testing.T) {
	t.Parallel()

	now := time.Now()
	collector := NewCollector()
	collector.Handle(context.Background(), RuntimeEvent{
		Time: now.Add(-time.Second), AgentID: "executor", Kind: KindModel, Phase: PhaseStart,
	})
	before := collector.Snapshot().ModelStreamIdle
	if before < 900*time.Millisecond || before > 2*time.Second {
		t.Fatalf("idle before activity = %s", before)
	}
	collector.Handle(context.Background(), RuntimeEvent{
		Time: now.Add(-100 * time.Millisecond), AgentID: "executor", Kind: KindModel, Phase: PhaseDelta,
	})
	after := collector.Snapshot().ModelStreamIdle
	if after < 90*time.Millisecond || after > 500*time.Millisecond {
		t.Fatalf("idle after activity = %s", after)
	}
	collector.Handle(context.Background(), RuntimeEvent{
		Time: now, AgentID: "executor", Kind: KindModel, Phase: PhaseEnd,
	})
	if idle := collector.Snapshot().ModelStreamIdle; idle != 0 {
		t.Fatalf("idle after completion = %s", idle)
	}
}

func TestCollectorAccumulatesCachedTokens(t *testing.T) {
	t.Parallel()

	collector := NewCollector()
	collector.Handle(context.Background(), ModelEnd(
		"planner", "gpt-5", time.Now().Add(-time.Millisecond), 0, 1500, 800, nil,
	))
	collector.Handle(context.Background(), ModelEnd(
		"verifier", "gpt-5", time.Now().Add(-time.Millisecond), 0, 500, 0, nil,
	))
	got := collector.Snapshot()
	if got.Tokens != 2000 || got.CachedTokens != 800 {
		t.Fatalf("usage = tokens %d cached %d, want 2000/800", got.Tokens, got.CachedTokens)
	}
}
