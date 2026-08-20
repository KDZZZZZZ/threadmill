package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestResponsesGenerateCallsResponsesAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("request path = %q, want /v1/responses", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}

		var got map[string]any
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"model":        "gpt-5",
			"instructions": "system prompt",
			"store":        false,
			"include":      []any{"reasoning.encrypted_content"},
			"input": []any{
				map[string]any{
					"role":    "system",
					"content": "system prompt",
				},
				map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "input_text",
						"text": "weather",
					}},
				},
			},
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "weather",
				"description": "Get weather",
				"parameters": map[string]any{
					"type": "object",
				},
				"strict": false,
			}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("request body = %#v, want %#v", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"resp_1",
  "status":"completed",
  "output":[{
    "id":"rs_1",
    "type":"reasoning",
    "encrypted_content":"opaque-reasoning",
    "summary":[{"type":"summary_text","text":"need weather"}]
  },{
    "id":"fc_1",
    "type":"function_call",
    "call_id":"call_1",
    "name":"weather",
    "arguments":"{\"city\":\"Paris\"}",
    "status":"completed"
  }],
  "usage":{
    "input_tokens":1200,
    "input_tokens_details":{"cached_tokens":800,"cache_write_tokens":10},
    "output_tokens":300,
    "output_tokens_details":{"reasoning_tokens":100},
    "total_tokens":1500
  }
}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	got, err := model.Generate(context.Background(), agent.Request{
		SystemPrompt: "system prompt",
		Messages:     []agent.Message{{Role: agent.RoleUser, Content: "weather"}},
		Tools: []agenttool.Definition{{
			Name:        "weather",
			Description: "Get weather",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := agent.AssistantMessage{
		Thinking:   "need weather",
		StopReason: agent.StopReasonToolUse,
		API:        OpenAIResponses,
		Provider:   OpenAIResponses,
		Model:      "gpt-5",
		ToolCalls: []agenttool.Call{{
			ID:        "call_1",
			Name:      "weather",
			Arguments: json.RawMessage(`{"city":"Paris"}`),
		}},
		ModelData: json.RawMessage(`[{"id":"rs_1","type":"reasoning","encrypted_content":"opaque-reasoning","summary":[{"type":"summary_text","text":"need weather"}]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`),
		Usage: &agent.Usage{
			InputTokens:      1200,
			CachedTokens:     800,
			CacheWriteTokens: 10,
			OutputTokens:     300,
			ReasoningTokens:  100,
			TotalTokens:      1500,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Generate() = %#v, want %#v", got, want)
	}
}

func TestResponsesGenerateReplaysProviderOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"model":   "gpt-5",
			"store":   false,
			"include": []any{"reasoning.encrypted_content"},
			"input": []any{
				map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type": "input_text",
						"text": "weather",
					}},
				},
				map[string]any{
					"id":                "rs_1",
					"type":              "reasoning",
					"encrypted_content": "opaque-reasoning",
					"summary":           []any{},
				},
				map[string]any{
					"id":        "fc_1",
					"type":      "function_call",
					"call_id":   "call_1",
					"name":      "weather",
					"arguments": `{"city":"Paris"}`,
					"status":    "completed",
				},
				map[string]any{
					"type":    "function_call_output",
					"call_id": "call_1",
					"output":  "sunny",
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("request body = %#v, want %#v", got, want)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"resp_2",
  "status":"completed",
  "output":[{
    "id":"msg_1",
    "type":"message",
    "role":"assistant",
    "content":[{"type":"output_text","text":"sunny","annotations":[]}],
    "status":"completed"
  }]
}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	state := json.RawMessage(`[{"id":"rs_1","type":"reasoning","encrypted_content":"opaque-reasoning","summary":[]},{"id":"fc_1","type":"function_call","call_id":"call_1","name":"weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}]`)
	got, err := model.Generate(context.Background(), agent.Request{Messages: []agent.Message{
		{Role: agent.RoleUser, Content: "weather"},
		{
			Role:      agent.RoleAssistant,
			ToolCalls: []agenttool.Call{{ID: "call_1", Name: "weather"}},
			ModelData: state,
		},
		{
			Role:    agent.RoleTool,
			Content: "sunny",
			ToolResult: &agenttool.Result{
				CallID:  "call_1",
				Name:    "weather",
				Content: "sunny",
				Details: json.RawMessage(`{"hidden":true}`),
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "sunny" {
		t.Fatalf("Generate().Content = %q, want sunny", got.Content)
	}
}

func TestResponsesGenerateReturnsRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "status":"completed",
  "output":[{
    "id":"msg_1",
    "type":"message",
    "role":"assistant",
    "content":[{"type":"refusal","refusal":"I cannot help with that."}],
    "status":"completed"
  }]
}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}

	got, err := model.Generate(context.Background(), agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "I cannot help with that." {
		t.Fatalf("Generate().Content = %q, want refusal", got.Content)
	}
	if got.StopReason != agent.StopReasonStop {
		t.Fatalf("Generate().StopReason = %q, want %q", got.StopReason, agent.StopReasonStop)
	}
}

func TestResponsesGenerateRetriesFiveTimesBeforeSuccess(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 5 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
			return
		}
		_, _ = w.Write([]byte(`{
  "status":"completed",
  "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]
}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	got, err := model.Generate(context.Background(), agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" {
		t.Fatalf("Generate().Content = %q, want ok", got.Content)
	}
	if got := requests.Load(); got != 6 {
		t.Fatalf("requests = %d, want one initial request plus five retries", got)
	}
}

func TestResponsesGenerateDoesNotRetryBadRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad input"}}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	_, err = model.Generate(context.Background(), agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("Generate() error = %v, want bad input", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestResponsesGenerateStopsAfterFiveRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"still unavailable"}}`))
	}))
	defer server.Close()

	model, err := NewResponses(testLLMConfig(t, server.URL+"/v1"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model.retryInterval = time.Millisecond
	_, err = model.Generate(context.Background(), agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "still unavailable") {
		t.Fatalf("Generate() error = %v, want final provider error", err)
	}
	if got := requests.Load(); got != 6 {
		t.Fatalf("requests = %d, want one initial request plus five retries", got)
	}
}
