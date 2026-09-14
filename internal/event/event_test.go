package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestNormalizeModelStart(t *testing.T) {
	got := ModelStart("manager", 3, 2)
	if got.Kind != KindModel || got.Phase != PhaseStart {
		t.Fatalf("kind/phase = %s/%s, want model/start", got.Kind, got.Phase)
	}
	if got.AgentID != "manager" || got.Messages != 3 || got.Tools != 2 {
		t.Fatalf("got %#v", got)
	}
	if got.Time.IsZero() {
		t.Fatal("time is zero")
	}
	if got.Duration != 0 || got.Err != "" {
		t.Fatalf("start should not carry duration or error: %#v", got)
	}
}

func TestNormalizeModelEnd(t *testing.T) {
	started := time.Now().Add(-12 * time.Millisecond)
	got := ModelEnd("planner", "deepseek", started, 1, 40, 32, nil)
	if got.Kind != KindModel || got.Phase != PhaseEnd {
		t.Fatalf("kind/phase = %s/%s, want model/end", got.Kind, got.Phase)
	}
	if got.Name != "deepseek" || got.ToolCalls != 1 || got.Tokens != 40 || got.CachedTokens != 32 {
		t.Fatalf("got %#v", got)
	}
	if got.Duration < 12*time.Millisecond {
		t.Fatalf("duration = %s, want at least 12ms", got.Duration)
	}
}

func TestNormalizeModelEndError(t *testing.T) {
	got := ModelEnd("planner", "", time.Time{}, 0, 0, 0, errors.New("provider down"))
	if got.Err != "provider down" || !got.IsError {
		t.Fatalf("error fields = %#v", got)
	}
}

func TestModelRetry(t *testing.T) {
	got := ModelRetry("planner", 2, "stream_server_error")
	if got.Kind != KindModel || got.Phase != PhaseRetry || got.AgentID != "planner" ||
		got.Retries != 2 || got.RetryReason != "stream_server_error" {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeToolPair(t *testing.T) {
	start := ToolStart("executor", "bash", "call-1")
	if start.Kind != KindTool || start.Phase != PhaseStart || start.Name != "bash" || start.CallID != "call-1" {
		t.Fatalf("start = %#v", start)
	}

	got := ToolEnd("executor", "bash", "call-1", time.Now().Add(-time.Millisecond), true, errors.New("exit 1"))
	if got.Kind != KindTool || got.Phase != PhaseEnd {
		t.Fatalf("kind/phase = %s/%s, want tool/end", got.Kind, got.Phase)
	}
	if !got.IsError || got.Err != "exit 1" || got.Duration <= 0 {
		t.Fatalf("end = %#v", got)
	}
}

func TestModelDelta(t *testing.T) {
	got := ModelDelta("manager", "Hel")
	if got.Kind != KindModel || got.Phase != PhaseDelta || got.AgentID != "manager" || got.Delta != "Hel" {
		t.Fatalf("got %#v", got)
	}
}

func TestReasoningDeltaStaysSeparateAndOutOfMonitoring(t *testing.T) {
	const text = " \nfixture\t"
	var got RuntimeEvent
	ctx := WithReasoningDeltaSink(context.Background(), func(delta string) {
		got = ModelReasoningDelta("manager", delta)
	})
	ReasoningDeltaSink(ctx)(text)
	if got.ReasoningDelta != text || got.Delta != "" || got.Kind != KindModel || got.Phase != PhaseDelta {
		t.Fatalf("event = %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["reasoning_delta"] != text || decoded["delta"] != nil {
		t.Fatalf("wire event = %s", encoded)
	}
	var logs bytes.Buffer
	Monitor(slog.New(slog.NewJSONHandler(&logs, nil)))(ctx, got)
	collector := NewCollector()
	collector.Handle(ctx, got)
	snapshot, err := json.Marshal(collector.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 || bytes.Contains(snapshot, []byte("fixture")) || bytes.Contains(snapshot, []byte("reasoning_delta")) {
		t.Fatalf("monitor retained reasoning: logs=%s snapshot=%s", logs.Bytes(), snapshot)
	}
	if ReasoningDeltaSink(nil) != nil || ReasoningDeltaSink(context.Background()) != nil || DeltaSink(ctx) != nil {
		t.Fatal("reasoning sink must be separate and absent by default")
	}
}

func TestDeltaSinkRoundTrip(t *testing.T) {
	var got []string
	ctx := WithDeltaSink(context.Background(), func(delta string) {
		got = append(got, delta)
	})
	sink := DeltaSink(ctx)
	if sink == nil {
		t.Fatal("missing sink")
	}
	sink("a")
	sink("b")
	if strings.Join(got, "") != "ab" {
		t.Fatalf("got %q", got)
	}
	if DeltaSink(context.Background()) != nil {
		t.Fatal("empty context should have no sink")
	}
}

func TestDeltaActivitySinkRoundTrip(t *testing.T) {
	var activities []bool
	ctx := WithDeltaActivitySink(context.Background(), func(text bool) {
		activities = append(activities, text)
	})
	sink := DeltaActivitySink(ctx)
	if sink == nil {
		t.Fatal("missing activity sink")
	}
	sink(false)
	sink(true)
	if len(activities) != 2 || activities[0] || !activities[1] {
		t.Fatalf("activities = %#v", activities)
	}
	if DeltaActivitySink(context.Background()) != nil {
		t.Fatal("empty context should have no activity sink")
	}
}

func TestRetrySinkRoundTrip(t *testing.T) {
	var reasons []string
	ctx := WithRetrySink(context.Background(), func(reason string) {
		reasons = append(reasons, reason)
	})
	sink := RetrySink(ctx)
	if sink == nil {
		t.Fatal("missing sink")
	}
	sink("transport")
	sink("stream_server_error")
	if len(reasons) != 2 || reasons[0] != "transport" || reasons[1] != "stream_server_error" {
		t.Fatalf("reasons = %q", reasons)
	}
	if RetrySink(context.Background()) != nil {
		t.Fatal("empty context should have no sink")
	}
}
