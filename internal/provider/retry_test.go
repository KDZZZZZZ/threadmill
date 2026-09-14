package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/event"
)

func TestResponsesWrappedTransientErrors(t *testing.T) {
	for _, test := range []struct {
		name, code, message string
		retry               bool
	}{
		{"upstream rate limit", "upstream_error", "too many rate-limited requests from this key; slow down and retry after Retry-After seconds", true},
		{"upstream stream disconnected", "upstream_error", "OpenAI stream disconnected before completion: filter Grok Responses billing ping: unexpected EOF", true},
		{"unrelated upstream EOF", "upstream_error", "invalid request document: unexpected EOF", false},
		{"RPM limit", "invalid_request", "客户端 API Key 已超过 RPM 限制", true},
		{"model channel unavailable", "new_api_error", "No available channel for model grok-4.6 under group group_1 (distributor)", true},
		{"upstream CPU overloaded", "new_api_error", "system cpu overloaded (current: 98.7%, threshold: 90%)", true},
		{"other gateway error", "new_api_error", "invalid API key", false},
		{"invalid model", "invalid_request", "unknown model", false},
		{"other upstream error", "upstream_error", "unsupported response format", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					body, _ := json.Marshal(map[string]any{"response": map[string]any{
						"status": "failed", "error": map[string]string{"code": test.code, "message": test.message},
					}})
					writeSSE(w, w.(http.Flusher), "response.failed", string(body))
					return
				}
				writeSSE(w, w.(http.Flusher), "response.completed", `{"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
			}))
			defer server.Close()
			model, err := NewResponses(testLLMConfig(t, server.URL), server.Client())
			if err != nil {
				t.Fatal(err)
			}
			model.retryInterval = time.Millisecond
			ctx := event.WithDeltaSink(t.Context(), func(string) {})
			message, err := model.Generate(ctx, agent.Request{})
			if test.retry {
				if err != nil || calls.Load() != 2 || message.Content != "ok" {
					t.Fatalf("calls=%d message=%q err=%v", calls.Load(), message.Content, err)
				}
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("nonretryable error: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestResponsesRetryAfterWaitCanBeCanceled(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, header := range []string{"30", time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)} {
			name := "http/" + header
			if stream {
				name = "stream/" + header
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Retry-After", header)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						writeSSE(w, w.(http.Flusher), "response.failed", `{"response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"wait"}}}`)
					} else {
						w.WriteHeader(http.StatusTooManyRequests)
					}
				}))
				defer server.Close()
				model, err := NewResponses(testLLMConfig(t, server.URL), server.Client())
				if err != nil {
					t.Fatal(err)
				}
				model.retryInterval = time.Millisecond
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				ctx = event.WithDeltaSink(ctx, func(string) {})
				_, err = model.Generate(ctx, agent.Request{})
				if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
					t.Fatalf("must wait for Retry-After until canceled: calls=%d err=%v", calls.Load(), err)
				}
			})
		}
	}
}

func TestResponsesActivityOnlyStreamRetriesWithoutResettingChat(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req createResponseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if !req.Stream {
			t.Error("activity-only request did not enable SSE")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, w.(http.Flusher), "response.output_text.delta", `{"delta":"hidden partial memory"}`)
		if calls.Add(1) == 1 {
			writeSSE(w, w.(http.Flusher), "response.failed", `{"response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"try later"}}}`)
			return
		}
		writeSSE(w, w.(http.Flusher), "response.completed", `{"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"memory result"}]}]}}`)
	}))
	defer server.Close()
	model, err := NewResponses(testLLMConfig(t, server.URL), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	var textActivity int
	ctx := event.WithDeltaActivitySink(t.Context(), func(text bool) {
		if text {
			textActivity++
		}
	})
	ctx = event.WithDeltaResetSink(ctx, func() { t.Error("hidden activity reset visible chat") })
	result, err := model.Generate(ctx, agent.Request{})
	if err != nil || calls.Load() != 2 || textActivity == 0 || result.Content != "memory result" {
		t.Fatalf("calls=%d activity=%d result=%q err=%v", calls.Load(), textActivity, result.Content, err)
	}
}
