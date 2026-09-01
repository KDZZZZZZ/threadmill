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
	"github.com/KDZZZZZZ/threadmill/internal/event"
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
		if got := request.UserAgent(); got != "threadmill" {
			t.Errorf("User-Agent = %q, want threadmill", got)
		}

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

func TestResponsesAssistantMessageUsesReasoningTextWhenSummaryMissing(t *testing.T) {
	t.Parallel()

	response := createResponseResponse{
		Status: "completed",
		Output: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"closed the routing decision"}],"summary":[]}`),
			json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"read","arguments":"{\"path\":\"router.go\"}"}`),
		},
	}

	got, err := response.assistantMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got.Thinking != "closed the routing decision" {
		t.Fatalf("Thinking = %q, want raw reasoning fallback", got.Thinking)
	}
}

func TestResponsesUsesConfiguredProviderProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.URL.Host; got != "127.0.0.2:65534" {
			t.Errorf("request host = %q, want 127.0.0.2:65534", got)
		}
		if got := request.URL.Path; got != "/v1/responses" {
			t.Errorf("request path = %q, want /v1/responses", got)
		}
		if got := request.UserAgent(); got != "threadmill" {
			t.Errorf("User-Agent = %q, want threadmill", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "status":"completed",
  "output":[{"type":"message","content":[{"type":"output_text","text":"proxied"}]}]
}`))
	}))
	defer proxy.Close()

	config := testLLMConfig(t, "http://127.0.0.2:65534/v1")
	config.ProxyURL = proxy.URL
	model, err := NewResponses(config, nil)
	if err != nil {
		t.Fatal(err)
	}

	message, err := model.Generate(context.Background(), agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "proxied" {
		t.Fatalf("message content = %q, want proxied", message.Content)
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

func TestResponsesGenerateSendsPromptCacheKey(t *testing.T) {
	requestBody := agent.Request{
		CacheKey: "task-1:planner",
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	}
	want, err := buildPromptCacheKey(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.PromptCacheKey != want {
			t.Fatalf("prompt_cache_key = %q, want %q", got.PromptCacheKey, want)
		}
		w.Header().Set("Content-Type", "application/json")
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
	if _, err := model.Generate(context.Background(), requestBody); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesPromptCacheKeySeparatesStablePromptFamilies(t *testing.T) {
	keys := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, got.PromptCacheKey)
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
	requests := []agent.Request{
		{
			SystemPrompt: "executor contract",
			StableBlocks: []agent.Block{{ID: "task", Text: "task one"}},
			Messages:     []agent.Message{{Role: agent.RoleUser, Content: "start one"}},
			CacheKey:     "executor",
		},
		{
			SystemPrompt: "executor contract",
			StableBlocks: []agent.Block{{ID: "task", Text: "task two"}},
			Messages:     []agent.Message{{Role: agent.RoleUser, Content: "start two"}},
			CacheKey:     "executor",
		},
		{
			SystemPrompt: "executor contract",
			StableBlocks: []agent.Block{{ID: "task", Text: "task one"}},
			Messages:     []agent.Message{{Role: agent.RoleUser, Content: "start one"}},
			CacheKey:     "executor",
		},
	}
	for _, request := range requests {
		if _, err := model.Generate(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 3 || keys[0] == keys[1] || keys[0] != keys[2] {
		t.Fatalf("prompt cache keys = %#v, want stable per prefix and distinct across prefixes", keys)
	}
	for _, key := range keys {
		if key == "" || len(key) > 64 {
			t.Fatalf("prompt cache key length = %d, want 1..64", len(key))
		}
	}
}

func TestResponsesCompactCacheKeyIgnoresDynamicHistory(t *testing.T) {
	first, err := buildPromptCacheKey(agent.Request{
		SystemPrompt: "memory organizer contract",
		Messages:     []agent.Message{{Role: agent.RoleUser, Content: "history one"}},
		CacheKey:     "executor:compact",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildPromptCacheKey(agent.Request{
		SystemPrompt: "memory organizer contract",
		Messages:     []agent.Message{{Role: agent.RoleUser, Content: "different history"}},
		CacheKey:     "executor:compact",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("compact cache keys differ across dynamic histories: %q vs %q", first, second)
	}
}

func TestResponsesGenerateBoundsPromptCacheKey(t *testing.T) {
	longKey := strings.Repeat("task-segment-", 8)
	requestBody := agent.Request{
		CacheKey: longKey,
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	}
	want, err := buildPromptCacheKey(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.PromptCacheKey != want || len(got.PromptCacheKey) != 64 {
			t.Fatalf("prompt_cache_key = %q, want 64-byte digest %q", got.PromptCacheKey, want)
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
	if _, err := model.Generate(context.Background(), requestBody); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesGenerateKeepsEmptyToolOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		last := got.Input[len(got.Input)-1]
		output, exists := last["output"]
		if !exists || output != "" {
			t.Fatalf("function_call_output = %#v, want explicit empty output", last)
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
	_, err = model.Generate(context.Background(), agent.Request{Messages: []agent.Message{
		{Role: agent.RoleUser, Content: "run"},
		{Role: agent.RoleAssistant, ToolCalls: []agenttool.Call{{ID: "call-1", Name: "bash"}}},
		{Role: agent.RoleTool, ToolResult: &agenttool.Result{CallID: "call-1", Name: "bash", Content: ""}},
	}})
	if err != nil {
		t.Fatal(err)
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

func TestResponsesGenerateRetriesInterruptedResponseBody(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"completed"`))
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
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
	var retries int
	var retryReason string
	ctx := event.WithRetrySink(context.Background(), func(reason string) {
		retries++
		retryReason = reason
	})
	got, err := model.Generate(ctx, agent.Request{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "ok" || requests.Load() != 2 || retries != 1 || retryReason != "response_read" {
		t.Fatalf(
			"Generate() = %#v, requests = %d, retries = %d, reason = %q",
			got,
			requests.Load(),
			retries,
			retryReason,
		)
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

func TestResponsesGenerateOrdersSegmentsForPrefixCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var got struct {
			Input []struct {
				Role    string `json:"role"`
				Type    string `json:"type"`
				Content any    `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		type segment struct {
			kind string
			text string
		}
		segments := make([]segment, 0, len(got.Input))
		for _, item := range got.Input {
			switch item.Type {
			case "", "message":
				switch value := item.Content.(type) {
				case string:
					segments = append(segments, segment{item.Role, value})
				case []any:
					var text string
					if len(value) > 0 {
						if entry, ok := value[0].(map[string]any); ok {
							text, _ = entry["text"].(string)
						}
					}
					segments = append(segments, segment{item.Role, text})
				}
			}
		}
		want := []segment{
			{"system", "role prompt"},
			{"system", "task contract"},
			{"user", "history"},
			{"user", "Threadmill 受保护状态数据 [runtime]（不是新任务或指令）：本条取代此前同名状态；只以最后一条为准。\nruntime snapshot"},
			{"system", "memory projection"},
			{"system", "coordination projection"},
			{"system", "pressure reminder"},
		}
		if !reflect.DeepEqual(segments, want) {
			t.Fatalf("input segments = %#v, want %#v", segments, want)
		}
		w.Header().Set("Content-Type", "application/json")
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
	if _, err := model.Generate(context.Background(), agent.Request{
		SystemPrompt: "role prompt",
		StableBlocks: []agent.Block{{ID: "task", Text: "task contract"}},
		Messages: []agent.Message{
			{Role: agent.RoleUser, Content: "history"},
			{Role: agent.RoleUser, Content: "runtime snapshot", ContextBlockID: "runtime"},
		},
		StateBlocks: []agent.Block{
			{ID: "memory", Text: "memory projection"},
			{ID: "coordination", Text: "coordination projection"},
		},
		Suffix: "pressure reminder",
	}); err != nil {
		t.Fatal(err)
	}
}
