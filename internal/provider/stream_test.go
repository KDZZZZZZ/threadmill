package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/event"
)

func TestResponsesGenerateStreamsDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		var got map[string]any
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got["stream"] != true {
			t.Errorf("stream = %#v, want true", got["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher")
		}
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"Hel"}`)
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"lo"}`)
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{
      "type":"message",
      "content":[{"type":"output_text","text":"Hello"}]
    }],
    "usage":{
      "input_tokens":3,
      "output_tokens":1,
      "total_tokens":4
    }
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	ctx := event.WithDeltaSink(context.Background(), func(delta string) {
		deltas = append(deltas, delta)
	})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %q", deltas)
	}
	if got.Content != "Hello" || got.StopReason != agent.StopReasonStop {
		t.Fatalf("message = %#v", got)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %#v", got.Usage)
	}
}

func TestResponsesGenerateStreamFailed(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.failed", `{
  "type":"response.failed",
  "response":{
    "status":"failed",
    "error":{"code":"missing_required_parameter","message":"input is required"}
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	ctx := event.WithDeltaSink(context.Background(), func(string) {})
	_, err = model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "missing_required_parameter: input is required") {
		t.Fatalf("error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want non-retryable failure once", requests.Load())
	}
}

func TestResponsesGenerateStreamRetriesFailedServerEvent(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if requests.Add(1) == 1 {
			writeSSE(w, flusher, "response.failed", `{
  "type":"response.failed",
  "response":{
    "status":"failed",
    "error":{"code":"server_error","message":"model failed"}
  }
}`)
			return
		}
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	var retryReason string
	ctx := event.WithReplayableDeltas(context.Background())
	ctx = event.WithRetrySink(ctx, func(reason string) {
		retries++
		retryReason = reason
	})
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 || retries != 1 || retryReason != "stream_server_error" {
		t.Fatalf(
			"Generate() = %#v, requests = %d, retries = %d, reason = %q",
			got,
			requests.Load(),
			retries,
			retryReason,
		)
	}
}

func TestResponsesGenerateStreamRetriesStreamReadErrorBeforeOutput(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if requests.Add(1) == 1 {
			writeSSE(w, flusher, "error", `{
  "type":"error",
  "error":{"code":"stream_read_error","message":"stream_read_error"}
}`)
			return
		}
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	var retryReason string
	ctx := event.WithRetrySink(context.Background(), func(reason string) {
		retries++
		retryReason = reason
	})
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 || retries != 1 || retryReason != "stream_read" {
		t.Fatalf(
			"Generate() = %#v, requests = %d, retries = %d, reason = %q",
			got,
			requests.Load(),
			retries,
			retryReason,
		)
	}
}

func TestResponsesGenerateStreamRetriesBeforeSSEStarts(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"try again"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	var retryReason string
	ctx := event.WithRetrySink(context.Background(), func(reason string) {
		retries++
		retryReason = reason
	})
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 || retries != 1 || retryReason != "http_rate_limit" {
		t.Fatalf(
			"Generate() = %#v, requests = %d, retries = %d, reason = %q",
			got,
			requests.Load(),
			retries,
			retryReason,
		)
	}
}

func TestResponsesGenerateStreamReturnsAtDoneMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"done"}`)
		_, _ = io.WriteString(w, "data: [DONE]\n")
		flusher.Flush()
		<-request.Context().Done()
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "done" {
		t.Fatalf("content = %q, want done", got.Content)
	}
}

func TestResponsesGenerateStreamReturnsAtCompletedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]
  }
}`)
		<-request.Context().Done()
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "done" {
		t.Fatalf("content = %q, want done", got.Content)
	}
}

func TestResponsesGenerateStreamRetriesInterruptedBodyBeforeDelta(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	ctx := event.WithRetrySink(context.Background(), func(string) { retries++ })
	ctx = event.WithDeltaSink(ctx, func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 || retries != 1 {
		t.Fatalf("Generate() = %#v, requests = %d, retries = %d", got, requests.Load(), retries)
	}
}

func TestResponsesGenerateStreamDoesNotReplayDeliveredDelta(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"partial"}`)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	var deltas []string
	ctx := event.WithDeltaSink(context.Background(), func(delta string) {
		deltas = append(deltas, delta)
	})
	_, err = model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
	if requests.Load() != 1 || strings.Join(deltas, "") != "partial" {
		t.Fatalf("requests = %d, deltas = %q", requests.Load(), deltas)
	}
}

func TestResponsesGenerateStreamRetriesReplayableDelta(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"partial"}`)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"complete"}`)
		writeSSE(w, flusher, "response.completed", `{
  "type":"response.completed",
  "response":{
    "status":"completed",
    "output":[{"type":"message","content":[{"type":"output_text","text":"complete"}]}]
  }
}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	var activities []bool
	var deltas []string
	ctx := event.WithRetrySink(context.Background(), func(string) { retries++ })
	ctx = event.WithDeltaActivitySink(ctx, func(text bool) { activities = append(activities, text) })
	ctx = event.WithDeltaSink(ctx, func(delta string) { deltas = append(deltas, delta) })
	ctx = event.WithReplayableDeltas(ctx)
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "complete" || requests.Load() != 2 || retries != 1 || len(activities) != 3 {
		t.Fatalf(
			"Generate() = %#v, requests = %d, retries = %d, activities = %d",
			got, requests.Load(), retries, len(activities),
		)
	}
	if !activities[0] || !activities[1] || activities[2] {
		t.Fatalf("activities = %#v, want text, text, completion", activities)
	}
	if strings.Join(deltas, "") != "complete" {
		t.Fatalf("deltas = %q, want only successful attempt", deltas)
	}
}

func TestResponsesGenerateStreamRetriesReaderErrorBeforeDelta(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if requests.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(iotest.ErrReader(
					errors.New("stream ID 23; INTERNAL_ERROR; received from peer"),
				)),
			}, nil
		}
		body := strings.NewReader(strings.Join([]string{
			`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`,
			``,
		}, "\n"))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(body)}, nil
	})}

	model, err := NewResponses(testLLMConfig(t, "https://example.com/v1"), client)
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	ctx := event.WithDeltaSink(context.Background(), func(string) {})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 {
		t.Fatalf("Generate() = %#v, requests = %d", got, requests.Load())
	}
}

func TestResponsesGenerateSharesRetryBudgetAcrossHTTPAndStreamFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := requests.Add(1)
		if attempt%6 != 0 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	retries := 0
	ctx := event.WithReplayableDeltas(context.Background())
	ctx = event.WithRetrySink(ctx, func(string) { retries++ })
	ctx = event.WithDeltaSink(ctx, func(string) {})
	_, err = model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "without response.completed") {
		t.Fatalf("Generate() error = %v", err)
	}
	if requests.Load() != 6 || retries != 5 {
		t.Fatalf("requests = %d, retries = %d; want one initial request plus 5 retries", requests.Load(), retries)
	}
}

func TestReadResponseStreamUsesTypeField(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"Hi"}`,
		``,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hi"}]}]}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var deltas []string
	got, err := readResponseStream(strings.NewReader(body), func(delta string) {
		deltas = append(deltas, delta)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "Hi" {
		t.Fatalf("deltas = %q", deltas)
	}
	message, err := got.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "Hi" {
		t.Fatalf("content = %q", message.Content)
	}
}

func TestReadResponseStreamReportsNonTextActivity(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"ping","arguments":"{}"}}`,
		``,
		`data: {"type":"response.completed","response":{"status":"completed","output":[]}}`,
		``,
	}, "\n")
	var activities []bool
	if _, err := readResponseStream(strings.NewReader(body), nil, func(text bool) {
		activities = append(activities, text)
	}); err != nil {
		t.Fatal(err)
	}
	if len(activities) != 3 || activities[0] || activities[1] || activities[2] {
		t.Fatalf("activities = %#v, want 3 non-text SSE events", activities)
	}
}

func TestReadResponseStreamReportsRetryableErrorDetails(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"error","error":{"code":"server_error","message":"busy"}}`,
		``,
	}, "\n")
	_, err := readResponseStream(strings.NewReader(body), nil, nil)
	if err == nil || err.Error() != "responses stream error: server_error: busy" {
		t.Fatalf("error = %v", err)
	}
	if !retryableResponseStreamError(err) {
		t.Fatal("server stream error is not retryable")
	}
}

func TestReadResponseStreamCompletedOmitsStatus(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"你好"}`,
		``,
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]}]}}`,
		``,
	}, "\n")
	got, err := readResponseStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := got.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "你好" {
		t.Fatalf("content = %q", message.Content)
	}
}

func TestReadResponseStreamCompletedEmptySnapshotUsesDeltas(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"你好"}`,
		``,
		`data: {"type":"response.completed","response":{}}`,
		``,
	}, "\n")
	got, err := readResponseStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := got.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "你好" {
		t.Fatalf("content = %q", message.Content)
	}
}

func TestReadResponseStreamKeepsCompletedOutputItemArguments(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}}`,
		``,
		`data: {"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_1","name":"ping","arguments":"{}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ping","arguments":"{}","status":"completed"}}`,
		``,
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ping","arguments":null,"status":"completed"}]}}`,
		``,
	}, "\n")

	got, err := readResponseStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := got.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || string(message.ToolCalls[0].Arguments) != `{}` {
		t.Fatalf("tool calls = %#v, want one call with complete arguments", message.ToolCalls)
	}
	if _, err := json.Marshal(message); err != nil {
		t.Fatalf("assistant message cannot be persisted: %v", err)
	}
}

func TestReadResponseStreamDoneKeepsCompletedOutputItem(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"ping","arguments":"{}","status":"completed"}}`,
		``,
		`data: [DONE]`,
	}, "\n")

	got, err := readResponseStream(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := got.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Name != "ping" {
		t.Fatalf("tool calls = %#v, want ping", message.ToolCalls)
	}
}

func TestReadResponseStreamMissingCompleted(t *testing.T) {
	_, err := readResponseStream(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "without response.completed") {
		t.Fatalf("error = %v", err)
	}
}

func TestProviderWithDeltaSink(t *testing.T) {
	var got string
	ctx := WithDeltaSink(context.Background(), func(delta string) {
		got = delta
	})
	sink := event.DeltaSink(ctx)
	if sink == nil {
		t.Fatal("missing sink")
	}
	sink("ok")
	if got != "ok" {
		t.Fatalf("got %q", got)
	}
}

func writeSSE(w io.Writer, flusher http.Flusher, eventName, data string) {
	data = strings.ReplaceAll(data, "\n", "")
	_, _ = io.WriteString(w, "event: "+eventName+"\n")
	_, _ = io.WriteString(w, "data: "+data+"\n\n")
	flusher.Flush()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
