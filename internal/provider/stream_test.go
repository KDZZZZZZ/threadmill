package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		writeSSE(w, flusher, "response.failed", `{"type":"response.failed"}`)
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := event.WithDeltaSink(context.Background(), func(string) {})
	_, err = model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "response.failed") {
		t.Fatalf("error = %v", err)
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
	})
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

func TestReadResponseStreamCompletedOmitsStatus(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"你好"}`,
		``,
		`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]}]}}`,
		``,
	}, "\n")
	got, err := readResponseStream(strings.NewReader(body), nil)
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
	got, err := readResponseStream(strings.NewReader(body), nil)
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

	got, err := readResponseStream(strings.NewReader(body), nil)
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

func TestReadResponseStreamMissingCompleted(t *testing.T) {
	_, err := readResponseStream(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"), nil)
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
