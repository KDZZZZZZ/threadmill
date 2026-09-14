//go:build integration

package coordination

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const liveMemoryMarker = "THREADMILL_GRAPH_MEM_7f3a"

func TestLiveSingleSourceInputKeepsMemoryAndFilesPaired(t *testing.T) {
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

	graph := New()
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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	files := vfs.NewStore(t.TempDir())
	t.Cleanup(func() { cancel(); graph.WaitRuns(); _ = files.Close() })
	graph.SetRunContext(ctx)
	if _, err := graph.ReplacePending(ctx, PendingSubgraph{
		Tasks: []PendingTask{
			{ID: "source", Info: liveGraphQuery(liveMemoryMarker)},
			{ID: "consumer", Info: liveGraphQuery(liveMemoryMarker)},
		},
		Edges: []Edge{{From: "source:1:verifier", To: "consumer:1:planner"}},
	}); err != nil {
		t.Fatal(err)
	}
	source, _ := graph.Task("source")
	consumer, _ := graph.Task("consumer")
	store := ctxgraph.NewStore()
	seed := liveSeedGraph(liveMemoryMarker)
	if err := store.Save(source.Env.ID, seed); err != nil {
		t.Fatal(err)
	}
	if err := files.View(source.Env.ID).Write("seed.txt", []byte(liveMemoryMarker)); err != nil {
		t.Fatal(err)
	}
	stores := Stores{Memory: store, Files: files}
	assemble := Assemble(stores, recorder, cfg.Agents, nil, cfg.LLM.ContextWindow, react,
		agent.FileOverlay{Tools: cfg.Tools, Prompts: cfg.Prompts})
	if got, err := graph.Run(ctx, source.ID, source.Info, stores, assemble); err != nil || strings.TrimSpace(got) == "" {
		t.Fatalf("source Run() = %q, %v", got, err)
	}
	sourceOutput, ok := graph.Output(source.Verifier.ID)
	if !ok {
		t.Fatal("source verifier has no committed paired output")
	}
	sourceMemory, ok := store.Snapshot(sourceOutput.MemoryRef)
	if !ok || !liveSeedWasOrganized(sourceMemory) || !recorder.called("organize_subgraph") {
		t.Fatalf("source did not organize seeded memory: %+v; calls=%v", sourceMemory, recorder.snapshot())
	}
	// Later source writes must not change the input selected by the ordinary edge.
	const lateSourceMarker = "source changed after its output committed"
	if err := store.Save(source.Env.ID, store.Load(source.Env.ID).WithMemory([]ctxgraph.Node{{
		ID: "source-later", Kind: ctxgraph.NodeKindFact, Statement: lateSourceMarker, Status: ctxgraph.NodeStatusAccepted,
	}}, nil)); err != nil {
		t.Fatal(err)
	}
	if err := files.View(source.Env.ID).Write("seed.txt", []byte(lateSourceMarker)); err != nil {
		t.Fatal(err)
	}
	got, err := graph.Run(ctx, consumer.ID, consumer.Info, stores, assemble)
	if err != nil || strings.TrimSpace(got) == "" {
		t.Fatalf("consumer Run() = %q, %v", got, err)
	}
	t.Logf("consumer verifier output: %s; tool calls: %v", got, recorder.snapshot())
	consumerMemory := store.Load(consumer.Env.ID)
	if !liveSeedWasOrganized(consumerMemory) || graphHasStatement(consumerMemory, lateSourceMarker) {
		t.Fatalf("consumer did not inherit the committed source memory: %+v", consumerMemory)
	}
	if current, _ := store.Snapshot(sourceOutput.MemoryRef); !reflect.DeepEqual(current, sourceMemory) {
		t.Fatal("consumer execution changed the immutable source memory")
	}
	for _, ref := range []string{sourceOutput.FilesRef, consumer.Env.ID} {
		if content, err := files.View(ref).Read("seed.txt"); err != nil || string(content) != liveMemoryMarker {
			t.Fatalf("file snapshot %q = %q, %v", ref, content, err)
		}
	}
	// Completed role journals clear; ready inputs stay available after reopening.
	restoredProgress, err := NewDirProgressStore(progressDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []Task{source, consumer} {
		state, ok, err := restoredProgress.Load(task.Env.ID)
		if err != nil || !ok || state.Pending != nil || len(state.Inputs) != 3 {
			t.Fatalf("completed task %s progress = %+v, %v, %v", task.ID, state, ok, err)
		}
		for _, input := range state.Inputs {
			if input.Phase != "ready" || !input.Started || input.FilesRef == "" || input.MemoryRef == "" {
				t.Fatalf("input pair is not ready: %+v", input)
			}
			memory, ok := store.Snapshot(input.MemoryRef)
			if !ok {
				t.Fatalf("ready memory is missing: %s", input.MemoryRef)
			}
			if err := files.Restore(input.FilesRef); err != nil {
				t.Fatalf("ready files are missing: %s: %v", input.FilesRef, err)
			}
			if input.NodeID == consumer.Planner.ID {
				if len(input.Sources) != 1 || input.Sources[0].ID != sourceOutput.Node.ID ||
					input.Sources[0].FilesRef != sourceOutput.FilesRef || input.Sources[0].MemoryRef != sourceOutput.MemoryRef {
					t.Fatalf("consumer did not retain its single paired source: %+v", input.Sources)
				}
				if !reflect.DeepEqual(memory, sourceMemory) {
					t.Fatal("single-source ready input reorganized the source memory")
				}
			}
		}
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
	return "记忆依赖：" + marker + "。记忆图里已有一条事实，陈述包含该标记。" +
		"必须调用 organize_subgraph，query 使用该标记，把相关节点整理进工具返回的子图。" +
		"规划、执行、核验都基于这条记忆；核验结论里写上该标记。"
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

func liveSeedWasOrganized(graph ctxgraph.Graph) bool {
	for _, node := range graph.Nodes {
		if node.ID != "n-seed" || !strings.Contains(node.Statement, liveMemoryMarker) {
			continue
		}
		for _, subgraph := range graph.Subgraphs {
			if subgraph.Kind == ctxgraph.SubgraphKindTask && slices.Contains(node.SubgraphIDs, subgraph.ID) {
				return true
			}
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
	return slices.Contains(p.toolCalls, name)
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
