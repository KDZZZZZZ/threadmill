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

func TestResponsesGenerateStreamsReasoningSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.reasoning_summary_text.delta", `{"delta":"summary fixture"}`)
		writeSSE(w, flusher, "response.reasoning_text.delta", `{"output_index":0,"content_index":0,"delta":" \n"}`)
		writeSSE(w, flusher, "response.reasoning_text.delta", `{"output_index":0,"content_index":0,"delta":"literal fixture\t"}`)
		writeSSE(w, flusher, "response.output_text.delta", `{"delta":"answer fixture"}`)
		writeSSE(w, flusher, "response.completed", `{"response":{"status":"completed","output":[
			{"type":"reasoning","encrypted_content":"opaque fixture","summary":[{"type":"summary_text","text":"summary fixture"}],"content":[{"type":"reasoning_text","text":" \nliteral fixture\t"}]},
			{"type":"message","content":[{"type":"output_text","text":"answer fixture"}]}
		]}}`)
	}))
	defer server.Close()
	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	var body, reasoning strings.Builder
	ctx := event.WithDeltaSink(context.Background(), func(delta string) { body.WriteString(delta) })
	ctx = event.WithReasoningDeltaSink(ctx, func(delta string) { reasoning.WriteString(delta) })
	message, err := model.Generate(ctx, agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if reasoning.String() != " \nliteral fixture\t" {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	if body.String() != "answer fixture" || message.Content != "answer fixture" || message.Thinking != "summary fixture" {
		t.Fatalf("body = %q, message = %#v", body.String(), message)
	}
}

func TestResponsesGenerateReasoningCompletion(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
		output string
		want   string
	}{
		{
			name:   "completed content without deltas",
			output: `{"type":"reasoning","content":[{"type":"reasoning_text","text":" \n"},{"type":"reasoning_text","text":"fixture\t"}]}`,
			want:   " \nfixture\t",
		},
		{
			name:   "completed content fills only missing suffix",
			prefix: "data: {\"type\":\"response.reasoning_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"fixture\"}\n\n",
			output: `{"type":"reasoning","content":[{"type":"reasoning_text","text":"fixture\t"},{"type":"reasoning_text","text":"\n"}]}`,
			want:   "fixture\t\n",
		},
		{
			name:   "done event and completed content do not duplicate",
			prefix: "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"fixture\"}\n\ndata: {\"type\":\"response.reasoning_text.done\",\"text\":\"fixture\\t\"}\n\n",
			output: `{"type":"reasoning","content":[{"type":"reasoning_text","text":"fixture\t"}]}`,
			want:   "fixture\t",
		},
		{
			name:   "summary and encrypted content are unavailable",
			prefix: "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"summary fixture\"}\n\n",
			output: `{"type":"reasoning","summary":[{"type":"summary_text","text":"summary fixture"}],"encrypted_content":"opaque fixture","content":[{"type":"summary_text","text":"content summary fixture"}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept") != "text/event-stream" {
					t.Error("reasoning-only sink must request SSE")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.prefix)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":["+test.output+`,{"type":"message","content":[{"type":"output_text","text":"answer fixture"}]}]}}`+"\n\n")
			}))
			defer server.Close()
			model, err := NewResponses(testLLMConfig(t, server.URL), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var reasoning strings.Builder
			ctx := event.WithReasoningDeltaSink(context.Background(), func(delta string) { reasoning.WriteString(delta) })
			message, err := model.Generate(ctx, agent.Request{})
			if err != nil {
				t.Fatal(err)
			}
			if reasoning.String() != test.want || message.Content != "answer fixture" {
				t.Fatalf("reasoning = %q, content = %q", reasoning.String(), message.Content)
			}
		})
	}
}

func TestResponsesGenerateReasoningRetryAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		replayable bool
		cancel     bool
		incomplete bool
		want       string
		requests   int32
	}{
		{name: "delivered reasoning prevents retry", want: "partial\t", requests: 1},
		{name: "buffered reasoning retries without duplicates", replayable: true, want: "complete\n", requests: 2},
		{name: "canceled stream stops delivery", cancel: true, want: "partial\t", requests: 1},
		{name: "incomplete stream discards buffered reasoning", replayable: true, incomplete: true, requests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				if requests.Add(1) == 1 {
					writeSSE(w, flusher, "response.reasoning_text.delta", `{"delta":"partial\t"}`)
					if test.cancel {
						writeSSE(w, flusher, "response.reasoning_text.delta", `{"delta":"after cancellation"}`)
						writeSSE(w, flusher, "response.completed", `{"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer fixture"}]}]}}`)
						return
					}
					if test.incomplete {
						writeSSE(w, flusher, "response.incomplete", `{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)
						return
					}
					writeSSE(w, flusher, "response.failed", `{"error":{"code":"server_error","message":"fixture error"}}`)
					return
				}
				writeSSE(w, flusher, "response.reasoning_text.delta", `{"delta":"complete\n"}`)
				writeSSE(w, flusher, "response.completed", `{"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer fixture"}]}]}}`)
			}))
			defer server.Close()
			model, err := NewResponses(testLLMConfig(t, server.URL), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			model.retryInterval = time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reasoning strings.Builder
			ctx = event.WithReasoningDeltaSink(ctx, func(delta string) {
				reasoning.WriteString(delta)
				if test.cancel {
					cancel()
				}
			})
			if test.replayable {
				ctx = event.WithReplayableDeltas(ctx)
			}
			message, err := model.Generate(ctx, agent.Request{})
			if test.replayable && !test.incomplete {
				if err != nil || message.Content != "answer fixture" {
					t.Fatalf("message = %#v, error = %v", message, err)
				}
			} else if err == nil {
				t.Fatal("interrupted stream succeeded")
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if reasoning.String() != test.want || requests.Load() != test.requests {
				t.Fatalf("reasoning = %q, requests = %d", reasoning.String(), requests.Load())
			}
		})
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

func TestResponsesGenerateRetriesGatewayConcurrencyLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if requests.Add(1) == 1 {
			writeSSE(w, flusher, "response.failed", `{"response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for account, please retry later"}}}`)
			return
		}
		writeSSE(w, flusher, "response.completed", `{"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"recovered"}]}]}}`)
	}))
	defer server.Close()
	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	var reason string
	ctx := event.WithRetrySink(t.Context(), func(value string) { reason = value })
	ctx = event.WithDeltaSink(ctx, func(string) {})
	message, err := model.Generate(ctx, agent.Request{Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}}})
	if err != nil || message.Content != "recovered" || requests.Load() != 2 || reason != "stream_rate_limit" {
		t.Fatalf("message=%q requests=%d reason=%q err=%v", message.Content, requests.Load(), reason, err)
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
	}, nil, nil)
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
	}, nil); err != nil {
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
	_, err := readResponseStream(strings.NewReader(body), nil, nil, nil)
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
	got, err := readResponseStream(strings.NewReader(body), nil, nil, nil)
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
	got, err := readResponseStream(strings.NewReader(body), nil, nil, nil)
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

	got, err := readResponseStream(strings.NewReader(body), nil, nil, nil)
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

	got, err := readResponseStream(strings.NewReader(body), nil, nil, nil)
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
	_, err := readResponseStream(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"), nil, nil, nil)
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

func TestResponsesRetriesResettableDisplayedStream(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if requests.Add(1) == 1 {
			writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"discard this partial"}`)
			writeSSE(w, flusher, "error", `{"type":"error","code":"stream_read_error","message":"stream_read_error"}`)
			return
		}
		writeSSE(w, flusher, "response.output_text.delta", `{"type":"response.output_text.delta","delta":"recovered"}`)
		writeSSE(w, flusher, "response.completed", `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"recovered"}]}]}}`)
	}))
	defer s.Close()
	m, err := NewResponses(testLLMConfig(t, s.URL), s.Client())
	if err != nil {
		t.Fatal(err)
	}
	m.retryInterval = time.Millisecond
	var visible string
	resets := 0
	ctx := event.WithDeltaSink(t.Context(), func(text string) { visible += text })
	ctx = event.WithDeltaResetSink(ctx, func() { visible = ""; resets++ })
	got, err := m.Generate(ctx, agent.Request{Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}}})
	if err != nil || got.Content != "recovered" || visible != "recovered" || resets != 1 || requests.Load() != 2 {
		t.Fatalf("reply=%q visible=%q resets=%d calls=%d err=%v", got.Content, visible, resets, requests.Load(), err)
	}
}
