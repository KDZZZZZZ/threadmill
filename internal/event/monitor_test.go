package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/logging"
)

func TestMonitorLogsRuntimeEvent(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Output: &output, JSON: true})
	bus := NewBus(Monitor(logger))

	bus.Publish(context.Background(), ToolEnd(
		"executor",
		"bash",
		"call-9",
		time.Now().Add(-2*time.Millisecond),
		false,
		nil,
	))

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode log: %v\n%s", err, output.String())
	}
	if got["msg"] != "runtime event" {
		t.Fatalf("msg = %v", got["msg"])
	}
	if got["kind"] != "tool" || got["phase"] != "end" {
		t.Fatalf("kind/phase = %v/%v", got["kind"], got["phase"])
	}
	if got["agent_id"] != "executor" || got["name"] != "bash" || got["call_id"] != "call-9" {
		t.Fatalf("ids = %#v", got)
	}
}

func TestMonitorLogsErrorLevel(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Output: &output, JSON: true})
	Monitor(logger)(context.Background(), ModelEnd("planner", "", time.Time{}, 0, 0, errors.New("timeout")))

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if got["level"] != "ERROR" || got["error"] != "timeout" {
		t.Fatalf("log = %#v", got)
	}
}

func TestMonitorLogsModelRetries(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Output: &output, JSON: true})
	ev := ModelRetry("planner", 3, "stream_server_error")
	Monitor(logger)(context.Background(), ev)

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if got["level"] != "WARN" || got["phase"] != "retry" ||
		got["retries"] != float64(3) || got["retry_reason"] != "stream_server_error" {
		t.Fatalf("retry log = %#v", got)
	}
}

func TestMonitorLogsMemoryOrganizerSelection(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Output: &output, JSON: true})
	ev := MemoryOrganized(
		"subgraph-organizer",
		"organize_task_context",
		"task-2-package",
		time.Now().Add(-time.Millisecond),
		40,
		0,
		nil,
	)
	Monitor(logger)(context.Background(), ev)

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if got["memory_candidates"] != float64(40) ||
		got["memory_selected"] != float64(0) {
		t.Fatalf("organizer log = %#v", got)
	}
}

func TestMonitorSkipsDelta(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Output: &output, JSON: true})
	Monitor(logger)(context.Background(), ModelDelta("manager", "tok"))
	if output.Len() != 0 {
		t.Fatalf("delta logged: %s", output.String())
	}
}

func TestMonitorNilLoggerUsesDefault(t *testing.T) {
	if Monitor(nil) == nil {
		t.Fatal("Monitor(nil) returned nil handler")
	}
}
