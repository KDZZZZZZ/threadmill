//go:build integration

package coordination

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
)

const liveMemoryMarker = "THREADMILL_GRAPH_MEM_7f3a"

func TestLiveGraphRunMemoryOpsAndEnvVersions(t *testing.T) {
	if os.Getenv("OPENCODE_API_KEY") == "" {
		t.Skip("OPENCODE_API_KEY is required for the live integration test")
	}

	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate integration test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))
	cfg, err := provider.LoadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	llm, err := provider.NewResponses(cfg.LLM, nil)
	if err != nil {
		t.Fatal(err)
	}

	recorder := &recordingProvider{
		inner: llm,
		log:   logging.New(logging.Config{Level: slog.LevelDebug}),
	}

	graph := newGraph()
	progressDir := t.TempDir()
	progress, err := NewDirProgressStore(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	graph.SetProgressStore(progress)

	reactDir := t.TempDir()
	react, err := agent.NewDirCheckpointStore(reactDir)
	if err != nil {
		t.Fatal(err)
	}

	store := ctxgraph.NewStore()
	rootTask := graph.AddTask()
	child := mustSpawn(t, graph, rootTask.Planner.ID, rootTask.Verifier.ID)
	seed := liveSeedGraph(liveMemoryMarker)
	store.Save(rootTask.Env.ID, seed)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	got, err := graph.Run(
		ctx,
		rootTask.ID,
		liveGraphQuery(liveMemoryMarker),
		store,
		Assemble(
			store,
			recorder,
			cfg.Agents,
			nil,
			cfg.LLM.ContextWindow,
			react,
		),
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("Run() returned empty verifier output")
	}

	parent := store.Load(rootTask.Env.ID)
	forked := store.Load(child.Env.ID)
	global := ctxgraph.Clone("check").Graph
	t.Logf("verifier output: %s", got)
	t.Logf("tool calls: %v", recorder.snapshot())
	t.Logf("parent revision=%d extra_subgraphs=%v extra_nodes=%v",
		parent.Revision, subgraphsNotIn(parent, seed), nodeSubgraphPairs(parent, seed))
	t.Logf("child revision=%d extra_subgraphs=%v extra_nodes=%v",
		forked.Revision, subgraphsNotIn(forked, seed), nodeSubgraphPairs(forked, seed))

	if !graphHasStatement(parent, liveMemoryMarker) {
		t.Fatal("parent env lost seeded memory")
	}
	if !graphHasStatement(forked, liveMemoryMarker) {
		t.Fatal("child env did not fork seeded memory")
	}
	if graphHasStatement(global, liveMemoryMarker) {
		t.Fatal("seeded memory leaked to the global graph")
	}
	if parent.Revision <= seed.Revision {
		t.Fatalf("parent revision = %d, want > %d after memory ops", parent.Revision, seed.Revision)
	}

	parentOnly := subgraphsNotIn(parent, seed)
	childOnly := subgraphsNotIn(forked, seed)
	if len(parentOnly) == 0 && !recorder.called("organize_subgraph") {
		t.Fatal("parent env gained no subgraph; organize_subgraph was not called")
	}
	for _, id := range childOnly {
		if containsString(parentOnly, id) {
			t.Fatalf("child subgraph %q leaked into parent env", id)
		}
		if subgraphByID(parent, id) {
			t.Fatalf("parent env saw child subgraph %q", id)
		}
	}
	for _, id := range parentOnly {
		if subgraphByID(forked, id) {
			t.Fatalf("child env saw parent subgraph %q written after fork", id)
		}
	}

	for _, node := range parent.Nodes {
		if nodeIn(seed, node.ID) {
			continue
		}
		for _, subgraphID := range node.SubgraphIDs {
			if !containsString(parentOnly, subgraphID) {
				continue
			}
			if nodeInSubgraph(forked, node.ID, subgraphID) {
				t.Fatalf("child env saw parent node %q in subgraph %q written after fork", node.ID, subgraphID)
			}
		}
	}
	for _, node := range forked.Nodes {
		if nodeIn(seed, node.ID) {
			continue
		}
		for _, subgraphID := range node.SubgraphIDs {
			if !containsString(childOnly, subgraphID) {
				continue
			}
			if nodeInSubgraph(parent, node.ID, subgraphID) {
				t.Fatalf("parent env saw child node %q in subgraph %q", node.ID, subgraphID)
			}
		}
	}

	entries, err := os.ReadDir(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("progress files after Run = %v, want discarded", names(entries))
	}
	reactEntries, err := os.ReadDir(reactDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reactEntries) != 0 {
		t.Fatalf("react checkpoints after Run = %v, want discarded", names(reactEntries))
	}
}

func liveGraphQuery(marker string) string {
	return "记忆图里已有一条事实，陈述包含标记 " + marker +
		"。必须调用 organize_subgraph，query 使用该标记，把相关节点整理进工具返回的子图。" +
		"规划、执行、核验都基于这条记忆；核验结论里写上该标记。" +
		"子任务同样必须调用 organize_subgraph。"
}

func liveSeedGraph(marker string) ctxgraph.Graph {
	return ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{
			ID:      "sg-seed",
			Name:    "seed",
			Summary: "seeded facts",
			Kind:    ctxgraph.SubgraphKindGeneral,
		}},
		Nodes: []ctxgraph.Node{{
			ID:          "n-seed",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "user preference marker " + marker,
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg-seed"},
		}},
	}
}

func graphHasStatement(graph ctxgraph.Graph, marker string) bool {
	for _, node := range graph.Nodes {
		if strings.Contains(node.Statement, marker) {
			return true
		}
	}
	return false
}

func subgraphsNotIn(graph, baseline ctxgraph.Graph) []string {
	var extra []string
	for _, subgraph := range graph.Subgraphs {
		if subgraphByID(baseline, subgraph.ID) {
			continue
		}
		extra = append(extra, subgraph.ID)
	}
	return extra
}

func subgraphByID(graph ctxgraph.Graph, id string) bool {
	for _, subgraph := range graph.Subgraphs {
		if subgraph.ID == id {
			return true
		}
	}
	return false
}

func nodeSubgraphPairs(graph, baseline ctxgraph.Graph) []string {
	var extra []string
	for _, node := range graph.Nodes {
		if node.ID == "" || nodeIn(baseline, node.ID) {
			continue
		}
		extra = append(extra, node.ID+":"+strings.Join(node.SubgraphIDs, ","))
	}
	return extra
}

func nodeIn(graph ctxgraph.Graph, id string) bool {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return true
		}
	}
	return false
}

func nodeInSubgraph(graph ctxgraph.Graph, nodeID, subgraphID string) bool {
	for _, node := range graph.Nodes {
		if node.ID == nodeID && containsString(node.SubgraphIDs, subgraphID) {
			return true
		}
	}
	return false
}

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

type recordingProvider struct {
	inner     agent.Provider
	log       *slog.Logger
	mu        sync.Mutex
	toolCalls []string
}

func (p *recordingProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	contentBytes := len(request.SystemPrompt)
	for _, message := range request.Messages {
		contentBytes += len(message.Content)
	}
	p.log.InfoContext(ctx, "model request",
		"tools", len(request.Tools),
		"messages", len(request.Messages),
		"content_bytes", contentBytes,
	)
	message, err := p.inner.Generate(ctx, request)
	if err != nil {
		p.log.ErrorContext(ctx, "model request failed", "error", err)
		return message, err
	}
	p.mu.Lock()
	for _, call := range message.ToolCalls {
		p.toolCalls = append(p.toolCalls, call.Name)
	}
	p.mu.Unlock()
	p.log.InfoContext(ctx, "model response",
		"tool_calls", len(message.ToolCalls),
		"content_bytes", len(message.Content),
		"content", clipForLog(message.Content, 800),
	)
	return message, nil
}

func (p *recordingProvider) called(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return containsString(p.toolCalls, name)
}

func (p *recordingProvider) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.toolCalls...)
}

func clipForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
