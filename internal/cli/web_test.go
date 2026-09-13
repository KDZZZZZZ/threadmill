package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/manager"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
)

func TestWebStreamsManagerMessagesAndDeduplicatesSubmissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	gateway, err := newWebGateway(ctx, "", "", func(ctx context.Context, opts manager.Options) (*manager.Manager, error) {
		opts.File = provider.FileConfig{LLM: provider.LLMConfig{Provider: provider.OpenAIResponses, Model: "local-test", ContextWindow: 32000}}
		opts.Provider = webTestProvider(func(ctx context.Context, _ agent.Request) (agent.AssistantMessage, error) {
			calls.Add(1)
			event.DeltaSink(ctx)("abandoned partial response")
			reset := event.DeltaResetSink(ctx)
			if reset == nil {
				t.Error("WebUI must provide reset capability")
				return agent.AssistantMessage{}, errors.New("missing reset")
			}
			reset()
			opts.OnEvent(ctx, event.ModelDelta("task-1:executor", "worker body must stay out of chat"))
			opts.OnEvent(ctx, event.ModelReasoningDelta("task-1:executor", "worker explicit trace\n保留原文"))
			if sink := event.ReasoningDeltaSink(ctx); sink != nil {
				sink("manager explicit trace\n保留原文")
			}
			if sink := event.DeltaSink(ctx); sink != nil {
				sink("partial ")
			}
			return agent.AssistantMessage{Content: "authoritative manager reply"}, nil
		})
		return manager.Open(ctx, opts)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	post := func(path, body string) map[string]any {
		t.Helper()
		response, err := http.Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode >= 300 {
			t.Fatalf("POST %s: %d %#v", path, response.StatusCode, result)
		}
		return result
	}
	body, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	project := post("/api/v1/projects", string(body))
	path := "/api/v1/projects/" + project["id"].(string)
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+path+"/events", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response = %s", response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	readEvent := func() (string, map[string]any) {
		t.Helper()
		var name string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				var value map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
					t.Fatal(err)
				}
				return name, value
			}
		}
		t.Fatalf("SSE ended: %v", scanner.Err())
		return "", nil
	}
	if name, value := readEvent(); name != "snapshot" || value["project_id"] != project["id"] {
		t.Fatalf("first SSE = %s %#v", name, value)
	}
	first := post(path+"/messages", `{"content":"hello","client_message_id":"browser-1"}`)
	second := post(path+"/messages", `{"content":"hello","client_message_id":"browser-1"}`)
	if first["message_id"] != second["message_id"] {
		t.Fatalf("different duplicate receipts: %#v %#v", first, second)
	}
	var deltaID string
	for {
		name, value := readEvent()
		data := value["data"].(map[string]any)
		if name == "runtime_event" && data["delta"] != nil {
			if data["agent_id"] != "manager" {
				t.Fatalf("worker body exposed: %#v", data)
			}
			deltaID, _ = value["message_id"].(string)
		}
		if name == "output" && data["speaker"] == "manager" {
			if data["content"] != "authoritative manager reply" || data["id"] != deltaID {
				t.Fatalf("final output did not reconcile delta: %#v (delta ID %q)", data, deltaID)
			}
			break
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d", calls.Load())
	}
	response.Body.Close()
	request, _ = http.NewRequestWithContext(ctx, "GET", server.URL+path+"/events", nil)
	request.Header.Set("Last-Event-ID", "0")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner = bufio.NewScanner(response.Body)
	name, value := readEvent()
	if name != "snapshot" {
		t.Fatalf("reconnect event = %s", name)
	}
	data := value["data"].(map[string]any)
	traces := data["reasoning"].(map[string]any)
	if traces["manager"].(map[string]any)["text"] != "manager explicit trace\n保留原文" || traces["task-1:executor"].(map[string]any)["text"] != "worker explicit trace\n保留原文" {
		t.Fatalf("reconnect reasoning changed: %#v", traces)
	}
	items := data["messages"].(map[string]any)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("reconnect messages = %#v", items)
	}
	for _, item := range data["agents"].([]any) {
		a := item.(map[string]any)
		if a["id"] == "task-1:executor" && (a["state"] != "running" || a["can_message"] != false) {
			t.Fatalf("worker state = %#v", a)
		}
	}
}

type webTestProvider func(context.Context, agent.Request) (agent.AssistantMessage, error)

func (f webTestProvider) Generate(ctx context.Context, req agent.Request) (agent.AssistantMessage, error) {
	return f(ctx, req)
}

func TestWebOpensOneManagerForConcurrentPathAliases(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gateway, err := newWebGateway(ctx, "", "", func(ctx context.Context, opts manager.Options) (*manager.Manager, error) {
		opens.Add(1)
		opts.Output("resumed manager reply")
		opts.File = provider.FileConfig{LLM: provider.LLMConfig{Provider: provider.OpenAIResponses, Model: "local-test", ContextWindow: 32000}}
		opts.Provider = webTestProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "ok"}, nil
		})
		return manager.Open(ctx, opts)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	if got := opens.Load(); got != 0 {
		t.Fatalf("opened %d managers before request", got)
	}
	const count = 6
	results := make(chan map[string]any, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := root
			if i%2 == 0 {
				path = alias
			}
			body, _ := json.Marshal(map[string]string{"root": path})
			resp, err := http.Post(server.URL+"/api/v1/projects", "application/json", strings.NewReader(string(body)))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				t.Errorf("open status %d", resp.StatusCode)
				return
			}
			var project map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
				t.Error(err)
				return
			}
			results <- project
		}()
	}
	wg.Wait()
	close(results)
	if got := opens.Load(); got != 1 {
		t.Fatalf("manager opens = %d, want 1", got)
	}
	var id any
	for project := range results {
		if id == nil {
			id = project["id"]
		}
		if project["id"] != id || project["root"] != root || project["model"] != "local-test" || project["manager_id"] != "manager" {
			t.Fatalf("project = %#v", project)
		}
	}
	resp, err := http.Get(server.URL + "/api/v1/projects/" + id.(string) + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct{ Items []struct{ Content string } }
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Content != "resumed manager reply" {
		t.Fatalf("startup outputs = %#v", page.Items)
	}
}

func TestWebRejectsUnsafeAndMalformedRequestsBeforeOpening(t *testing.T) {
	var opens atomic.Int32
	gateway, err := newWebGateway(context.Background(), "", "", func(context.Context, manager.Options) (*manager.Manager, error) {
		opens.Add(1)
		return nil, errSkipOpen
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, host, origin, contentType, body string
		status                                int
	}{
		{name: "DNS rebinding", host: "attacker.example:8787", body: `{"root":"/tmp"}`, status: 403},
		{name: "cross origin", origin: "https://attacker.example", body: `{"root":"/tmp"}`, status: 403},
		{name: "other local app", origin: "http://localhost:9000", body: `{"root":"/tmp"}`, status: 403},
		{name: "form submission", contentType: "text/plain", body: `{"root":"/tmp"}`, status: 415},
		{name: "unknown key", body: `{"root":"/tmp","recipient":"worker"}`, status: 400},
		{name: "trailing JSON", body: `{"root":"/tmp"} {}`, status: 400},
		{name: "oversized", body: `{"root":"` + strings.Repeat("x", 70<<10) + `"}`, status: 413},
		{name: "missing path", body: `{}`, status: 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/api/v1/projects", strings.NewReader(test.body))
			if test.host != "" {
				req.Host = test.host
			}
			if test.origin != "" {
				req.Header.Set("Origin", test.origin)
			}
			contentType := test.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			req.Header.Set("Content-Type", contentType)
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if opens.Load() != 0 {
		t.Fatalf("unsafe requests opened %d managers", opens.Load())
	}
}

func TestWebAcceptsConfiguredProxyOriginAndRejectsCrossOrigin(t *testing.T) {
	gateway, err := newWebGateway(context.Background(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	gateway.port = "8787"
	gateway.publicOrigin, err = url.Parse("https://machine.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, host, origin, site string
		status                   int
	}{
		{"proxy navigation", "machine.example.ts.net", "", "none", 200},
		{"proxy same origin", "machine.example.ts.net", "https://machine.example.ts.net", "same-origin", 200},
		{"local access", "127.0.0.1:8787", "http://127.0.0.1:8787", "same-origin", 200},
		{"wrong scheme", "machine.example.ts.net", "http://machine.example.ts.net", "", 403},
		{"foreign origin", "machine.example.ts.net", "https://attacker.example", "", 403},
		{"cross site without origin", "machine.example.ts.net", "", "cross-site", 403},
		{"foreign host", "attacker.example", "https://machine.example.ts.net", "same-origin", 403},
		{"wrong port", "machine.example.ts.net:9000", "", "", 403},
		{"null origin", "machine.example.ts.net", "null", "", 403},
		{"local other app", "127.0.0.1:9000", "", "", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/api/v1/projects", nil)
			req.Host = test.host
			req.Header.Set("Origin", test.origin)
			req.Header.Set("Sec-Fetch-Site", test.site)
			// Forwarded headers cannot grant access to an unconfigured Host.
			req.Header.Set("X-Forwarded-Host", "machine.example.ts.net")
			req.Header.Set("X-Forwarded-Proto", "https")
			rec := httptest.NewRecorder()
			gateway.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.status, rec.Body.String())
			}
		})
	}
	// Configuring a proxy must not change the default on another gateway.
	gateway.publicOrigin = nil
	req := httptest.NewRequest(http.MethodGet, "http://machine.example.ts.net/api/v1/projects", nil)
	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("default accepted external host: %d", rec.Code)
	}
}

func TestWebCancelCloseAndReopenProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 1)
	var opens atomic.Int32
	gateway, err := newWebGateway(ctx, "", "", func(ctx context.Context, opts manager.Options) (*manager.Manager, error) {
		opens.Add(1)
		opts.File = provider.FileConfig{LLM: provider.LLMConfig{Provider: provider.OpenAIResponses, Model: "local-test", ContextWindow: 32000}}
		opts.Provider = webTestProvider(func(ctx context.Context, _ agent.Request) (agent.AssistantMessage, error) {
			started <- struct{}{}
			<-ctx.Done()
			return agent.AssistantMessage{}, ctx.Err()
		})
		return manager.Open(ctx, opts)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	post := func(path string, body any, status int) map[string]any {
		t.Helper()
		data, _ := json.Marshal(body)
		resp, err := http.Post(server.URL+path, "application/json", strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var value map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s => %d %#v, want %d", path, resp.StatusCode, value, status)
		}
		return value
	}
	root := t.TempDir()
	project := post("/api/v1/projects", map[string]string{"root": root}, 201)
	path := "/api/v1/projects/" + project["id"].(string)
	post("/api/v1/projects", map[string]string{"root": root, "config_path": "other.yaml"}, 409)
	receipt := post(path+"/messages", map[string]string{"content": "wait", "client_message_id": "once"}, 202)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not receive message")
	}
	if response := post(path+"/cancel", map[string]any{}, 200); response["canceled"] != true {
		t.Fatalf("cancel = %#v", response)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			resp.Body.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		if state["busy"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled manager remained busy")
		}
		time.Sleep(time.Millisecond)
	}
	closed := post(path+"/close", map[string]any{}, 200)
	if closed["runtime_state"] != "closed" {
		t.Fatalf("close = %#v", closed)
	}
	post(path+"/messages", map[string]string{"content": "no", "client_message_id": "closed"}, 409)
	reopened := post("/api/v1/projects", map[string]string{"root": root}, 200)
	if reopened["id"] != project["id"] || opens.Load() != 2 {
		t.Fatalf("reopen = %#v, opens %d", reopened, opens.Load())
	}
	replayed := post(path+"/messages", map[string]string{"content": "wait", "client_message_id": "once"}, 202)
	if replayed["message_id"] != receipt["message_id"] {
		t.Fatalf("receipt changed after reopen: %#v %#v", receipt, replayed)
	}
}

func TestRunWebRejectsNonLocalBindingWithoutOpeningProject(t *testing.T) {
	var opens int
	code := Run([]string{"-web", "-listen", "0.0.0.0:8787"}, IO{Out: io.Discard, Err: io.Discard, Open: func(context.Context, manager.Options) (*manager.Manager, error) { opens++; return nil, errSkipOpen }})
	if code != 1 || opens != 0 {
		t.Fatalf("exit=%d opens=%d", code, opens)
	}
	opts, err := parse([]string{"-web"}, io.Discard)
	if err != nil || !opts.web || opts.listen != "127.0.0.1:8787" || opts.webUI != "docs/webui-demo.html" {
		t.Fatalf("defaults = %#v %v", opts, err)
	}
}

func TestRunWebRejectsInvalidProxyOrigin(t *testing.T) {
	for _, origin := range []string{
		"machine.example.ts.net", "ftp://machine.example.ts.net", "https://*.ts.net",
		"https://user:password@machine.example.ts.net", "https://machine.example.ts.net/path",
		"https://machine.example.ts.net?query", "https://machine.example.ts.net#fragment", "https://",
	} {
		t.Run(origin, func(t *testing.T) {
			var stderr strings.Builder
			code := Run([]string{"-web", "-web-origin", origin}, IO{Out: io.Discard, Err: &stderr})
			if code != 1 || !strings.Contains(stderr.String(), "-web-origin must be an exact") {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
		})
	}
	opts, err := parse([]string{"-web", "-web-origin", "https://machine.example.ts.net"}, io.Discard)
	if err != nil || opts.webOrigin != "https://machine.example.ts.net" {
		t.Fatalf("origin option = %q, error = %v", opts.webOrigin, err)
	}
}

func TestWebRejectsNewMessagesAfterManagerFailureAndAllowsReopen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opens int
	gateway, err := newWebGateway(ctx, "", "", func(ctx context.Context, opts manager.Options) (*manager.Manager, error) {
		opens++
		opts.File = provider.FileConfig{LLM: provider.LLMConfig{Provider: provider.OpenAIResponses, Model: "local-test", ContextWindow: 32000}}
		opts.Provider = webTestProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{}, errors.New("fixture terminal provider error")
		})
		return manager.Open(ctx, opts)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.close()
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:8787"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, req)
		return response
	}
	body, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	opened := request("POST", "/api/v1/projects", string(body))
	var project map[string]any
	if err := json.Unmarshal(opened.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + project["id"].(string)
	if response := request("POST", path+"/messages", `{"content":"first","client_message_id":"one"}`); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		response := request("GET", path, "")
		var state map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state["runtime_state"] == "error" {
			if !strings.Contains(state["error"].(string), "fixture terminal") {
				t.Fatalf("error state = %#v", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager never reported error: %s", response.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	// A surviving worker can enqueue its report after Manager has stopped.
	gateway.projects[project["id"].(string)].manager.Send("completed worker report")
	var afterReport webProjectView
	response := request("GET", path, "")
	if err := json.Unmarshal(response.Body.Bytes(), &afterReport); err != nil {
		t.Fatal(err)
	}
	if afterReport.Busy {
		t.Fatalf("unserviceable report presented as live work: %#v", afterReport)
	}
	if response := request("POST", path+"/messages", `{"content":"second","client_message_id":"two"}`); response.Code != 409 {
		t.Fatalf("dead manager accepted message: %s", response.Body.String())
	}
	if response := request("POST", "/api/v1/projects", string(body)); response.Code != 200 || opens != 2 {
		t.Fatalf("reopen: %s opens %d", response.Body.String(), opens)
	}
}

func TestWebShowsActivationRolesAndNestedActivity(t *testing.T) {
	g, err := newWebGateway(t.Context(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &webProject{id: "test", agents: map[string]webAgent{}, reasoning: map[string]webReasoning{}, subscribers: map[chan webEvent]struct{}{}}
	g.projects[p.id] = p
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: "analysis:2:executor", Kind: event.KindTool, Phase: event.PhaseStart, Name: "bash", CallID: "outer"})
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: "analysis:2:executor:checkpoint-3", Kind: event.KindModel, Phase: event.PhaseStart})
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: "analysis:2:executor:checkpoint-3", Kind: event.KindModel, Phase: event.PhaseEnd})
	r := httptest.NewRequest("GET", "http://localhost/api/v1/projects/test/agents", nil)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	var got struct {
		Items []webAgent `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "analysis:2:executor" || got.Items[0].Role != "executor" || got.Items[0].TaskID == nil || *got.Items[0].TaskID != "analysis" || got.Items[0].State != "running" {
		t.Fatalf("role disappeared or nested completion hid active call: %s", w.Body.String())
	}
}

func TestWebHelpWaitIsNotShownAsActiveExecution(t *testing.T) {
	g, err := newWebGateway(t.Context(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &webProject{id: "wait", agents: map[string]webAgent{}, reasoning: map[string]webReasoning{}, subscribers: map[chan webEvent]struct{}{}}
	g.projects[p.id] = p
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: "integration:1:executor", Kind: event.KindTool, Phase: event.PhaseStart, Name: "coordination_requestHelp", CallID: "help"})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/v1/projects/wait/agents", nil))
	var got struct {
		Items []webAgent `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].State != "waiting" {
		t.Fatalf("Help wait shown as running: %s", w.Body.String())
	}
}

func TestWebRetainsLiveRoleAfterFailedTaskIsResumed(t *testing.T) {
	taskHome := t.TempDir()
	t.Setenv("HOME", taskHome)
	root := t.TempDir()
	graph := coordination.New()
	task := graph.AddTask()
	snapshot := graph.Snapshot()
	snapshot.Tasks[0].Outcome = coordination.OutcomeFailed
	snapshot.Tasks[0].RunPolicy = coordination.RunPolicyHeld
	state, err := json.Marshal(map[string]any{"version": 1, "next_id": 1, "revision": snapshot.Revision, "tasks": snapshot.Tasks, "nodes": snapshot.Nodes, "edges": snapshot.Edges})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(taskHome, ".threadmill", "projects", fmt.Sprintf("%x", sha256.Sum256([]byte(root))), "graphs", "coordination.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, state, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := manager.Open(t.Context(), manager.Options{Root: root, File: provider.FileConfig{LLM: provider.LLMConfig{Provider: provider.OpenAIResponses, Model: "local-test", ContextWindow: 32000}}, Provider: webTestProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
		return agent.AssistantMessage{}, errors.New("held task must not call provider")
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	p := &webProject{manager: m, agents: map[string]webAgent{}, reasoning: map[string]webReasoning{}, subscribers: map[chan webEvent]struct{}{}}
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: task.Verifier.ID, Kind: event.KindMemory, Phase: event.PhaseStart, Name: "compact_memory"})
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: task.Verifier.ID, Kind: event.KindMemory, Phase: event.PhaseRetry, Name: "compact_memory", Retries: 1})
	p.refreshAgents()
	if got := p.agents[task.Verifier.ID]; got.State != "running" || got.Activity != "memory" {
		t.Fatalf("snapshot hid resumed role: %#v", got)
	}
	if got := p.agents[task.Executor.ID]; got.State != "failed" {
		t.Fatalf("inactive role lost task outcome: %#v", got)
	}
	p.onEvent(t.Context(), event.RuntimeEvent{AgentID: task.Verifier.ID, Kind: event.KindMemory, Phase: event.PhaseEnd, Name: "compact_memory"})
	p.refreshAgents()
	if got := p.agents[task.Verifier.ID]; got.State != "failed" {
		t.Fatalf("ended activity remained live: %#v", got)
	}
}
