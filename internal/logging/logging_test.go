package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewUsesInfoTextDefaults(t *testing.T) {
	var output bytes.Buffer
	logger := New(Config{Output: &output})

	logger.Debug("hidden")
	logger.Info("ready", "component", "agent")

	got := output.String()
	if strings.Contains(got, "hidden") {
		t.Fatalf("debug log was not filtered: %s", got)
	}
	if !strings.Contains(got, "level=INFO") ||
		!strings.Contains(got, "msg=ready") ||
		!strings.Contains(got, "component=agent") {
		t.Fatalf("info text log = %q", got)
	}
}

func TestNewSupportsJSONAndLevel(t *testing.T) {
	var output bytes.Buffer
	logger := New(Config{
		Output: &output,
		Level:  slog.LevelDebug,
		JSON:   true,
	})

	logger.Debug("model request", "step", 2)

	var got map[string]any
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON log: %v", err)
	}
	if got["level"] != "DEBUG" || got["msg"] != "model request" || got["step"] != float64(2) {
		t.Fatalf("JSON log = %#v", got)
	}
}
