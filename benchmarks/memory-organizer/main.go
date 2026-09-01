package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const evalEnvID = "organizer-eval"

// querySpec is one workload step. Group separates the five control queries the
// cold-start comparison depends on from the extended scenarios; Mode picks the
// production entry point exercised (query organizing, or the deep curation
// audit that compaction triggers in production).
type querySpec struct {
	ID      string     `json:"id"`
	Group   string     `json:"group,omitempty"`
	Mode    string     `json:"mode,omitempty"`
	Task    string     `json:"task"`
	Query   string     `json:"query"`
	Exclude string     `json:"exclude,omitempty"`
	Assert  assertions `json:"assert,omitempty"`
}

const (
	modeQuery  = "query"
	modeCurate = "curate"
)

type exchange struct {
	Started  time.Time              `json:"started"`
	Duration time.Duration          `json:"duration"`
	Request  agent.Request          `json:"request"`
	Response agent.AssistantMessage `json:"response"`
	Error    string                 `json:"error,omitempty"`
}

type recordingProvider struct {
	inner agent.Provider
	mu    sync.Mutex
	items []exchange
}

func (p *recordingProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	started := time.Now()
	response, err := p.inner.Generate(ctx, request)
	item := exchange{Started: started, Duration: time.Since(started), Request: request, Response: response}
	if err != nil {
		item.Error = err.Error()
	}
	p.mu.Lock()
	p.items = append(p.items, item)
	p.mu.Unlock()
	return response, err
}

func (p *recordingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}

func (p *recordingProvider) since(index int) []exchange {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]exchange(nil), p.items[index:]...)
}

type eventRecorder struct {
	mu    sync.Mutex
	items []event.RuntimeEvent
}

func (r *eventRecorder) handle(_ context.Context, ev event.RuntimeEvent) {
	r.mu.Lock()
	r.items = append(r.items, ev)
	r.mu.Unlock()
}

func (r *eventRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

func (r *eventRecorder) since(index int) []event.RuntimeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.RuntimeEvent(nil), r.items[index:]...)
}

type caseResult struct {
	Spec                 querySpec             `json:"spec"`
	CaseFile             string                `json:"case_file"`
	GraphNodesBefore     int                   `json:"graph_nodes_before"`
	GraphNodesAfter      int                   `json:"graph_nodes_after"`
	GraphSubgraphsBefore int                   `json:"graph_subgraphs_before"`
	GraphSubgraphsAfter  int                   `json:"graph_subgraphs_after"`
	SubscriptionsBefore  []string              `json:"subscriptions_before"`
	SubscriptionsAfter   []string              `json:"subscriptions_after"`
	Started              time.Time             `json:"started"`
	Duration             time.Duration         `json:"duration"`
	Subgraph             ctxgraph.Subgraph     `json:"subgraph"`
	Selected             []ctxgraph.Node       `json:"selected"`
	GraphDelta           graphDelta            `json:"graph_delta"`
	ToolOutput           string                `json:"tool_output,omitempty"`
	Metrics              event.MetricsSnapshot `json:"metrics"`
	Discipline           disciplineMetrics     `json:"discipline"`
	Projection           projectionCost        `json:"projection_cost"`
	SharedWithEarlier    int                   `json:"shared_with_earlier"`
	AssertionFailures    []string              `json:"assertion_failures,omitempty"`
	Degradation          []string              `json:"degradation,omitempty"`
	Events               []event.RuntimeEvent  `json:"events,omitempty"`
	Exchanges            []exchange            `json:"exchanges,omitempty"`
	Error                string                `json:"error,omitempty"`
}

// slim drops the full trace from a case. summary.json aggregates every case and
// grew O(n²) while holding a second copy of every request and response; the
// per-case file named by CaseFile stays the single home of the full trace.
func slim(result caseResult) caseResult {
	result.Events = nil
	result.Exchanges = nil
	return result
}

type runSummary struct {
	MemoryPath           string                `json:"memory_path"`
	SessionReset         bool                  `json:"organizer_session_reset"`
	Attribution          bool                  `json:"subscription_attribution"`
	SourceEnv            string                `json:"source_env"`
	SourceSHA256         string                `json:"source_sha256"`
	Model                string                `json:"model"`
	ContextWindow        int                   `json:"context_window"`
	InitialSubscriptions []string              `json:"initial_subscriptions"`
	FinalSubscriptions   []string              `json:"final_subscriptions"`
	FinalGraphNodes      int                   `json:"final_graph_nodes"`
	FinalGraphSubgraphs  int                   `json:"final_graph_subgraphs"`
	Started              time.Time             `json:"started"`
	Duration             time.Duration         `json:"duration"`
	Metrics              event.MetricsSnapshot `json:"metrics"`
	Cases                []caseResult          `json:"cases"`
	SourceUnmodified     bool                  `json:"source_unmodified"`
}

type nodeChange struct {
	Before ctxgraph.Node `json:"before"`
	After  ctxgraph.Node `json:"after"`
}

type subgraphChange struct {
	Before ctxgraph.Subgraph `json:"before"`
	After  ctxgraph.Subgraph `json:"after"`
}

// graphDelta captures every persistent organizer side effect, not just membership
// in the newly-created query subgraph. Later queries therefore expose whether
// opportunistic cleanup paid down noise in the same graph.
type graphDelta struct {
	RevisionBefore   int64               `json:"revision_before"`
	RevisionAfter    int64               `json:"revision_after"`
	NodesAdded       []ctxgraph.Node     `json:"nodes_added,omitempty"`
	NodesDeleted     []ctxgraph.Node     `json:"nodes_deleted,omitempty"`
	NodesChanged     []nodeChange        `json:"nodes_changed,omitempty"`
	SubgraphsAdded   []ctxgraph.Subgraph `json:"subgraphs_added,omitempty"`
	SubgraphsDeleted []ctxgraph.Subgraph `json:"subgraphs_deleted,omitempty"`
	SubgraphsChanged []subgraphChange    `json:"subgraphs_changed,omitempty"`
	EdgesAdded       []ctxgraph.Edge     `json:"edges_added,omitempty"`
	EdgesDeleted     []ctxgraph.Edge     `json:"edges_deleted,omitempty"`
}

type evalRunner struct {
	view          *ctxgraph.EnvView
	tool          agenttool.Tool
	organizer     *agent.Loop
	recording     *recordingProvider
	events        *eventRecorder
	subscriptions []string
	selected      map[string][]ctxgraph.Node
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var memoryPath, sourceEnv, queriesPath, outputDir, configRoot, configPath string
	var initialSubscriptions string
	var timeout time.Duration
	var sessionReset, attribution bool
	flag.StringVar(&memoryPath, "memory", "", "path to an archived memory.json")
	flag.StringVar(&sourceEnv, "env", "env-30", "source environment in memory.json")
	flag.StringVar(&queriesPath, "queries", "", "JSON query specification")
	flag.StringVar(&outputDir, "out", "", "directory for full traces and summary")
	flag.StringVar(&configRoot, "config-root", ".", "root used for layered runtime configuration")
	flag.StringVar(&configPath, "config", "", "optional highest-priority config override")
	flag.StringVar(
		&initialSubscriptions,
		"initial-subscriptions",
		"sg-q-1,sg-q-2",
		"comma-separated dynamic subgraphs initially visible to the requester",
	)
	flag.BoolVar(
		&sessionReset,
		"session-reset",
		true,
		"organizer drops history and re-instantiates from the graph under window pressure",
	)
	flag.BoolVar(
		&attribution,
		"subscription-attribution",
		false,
		"render the subscriber injection block grouped by source subgraph",
	)
	flag.DurationVar(&timeout, "timeout", 20*time.Minute, "timeout per organizer query")
	flag.Parse()
	if memoryPath == "" || queriesPath == "" || outputDir == "" {
		return errors.New("-memory, -queries and -out are required")
	}

	specs, err := readQuerySpecs(queriesPath)
	if err != nil {
		return err
	}
	config, err := provider.LoadRuntimeConfig(configRoot, configPath)
	if err != nil {
		return fmt.Errorf("load runtime config: %w", err)
	}
	model, err := provider.NewResponses(config.LLM, nil)
	if err != nil {
		return fmt.Errorf("create provider: %w", err)
	}
	beforeHash, err := fileSHA256(memoryPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	initial := splitIDs(initialSubscriptions)
	store, err := newEvalStore(memoryPath, sourceEnv)
	if err != nil {
		return err
	}
	runner, err := newEvalRunner(model, config, store, initial, runOptions{
		SessionReset: sessionReset,
		Attribution:  attribution,
	})
	if err != nil {
		return err
	}

	summary := runSummary{
		SessionReset:         sessionReset,
		Attribution:          attribution,
		MemoryPath:           memoryPath,
		SourceEnv:            sourceEnv,
		SourceSHA256:         beforeHash,
		Model:                config.LLM.Model,
		ContextWindow:        config.LLM.ContextWindow,
		InitialSubscriptions: append([]string(nil), initial...),
		Started:              time.Now(),
	}
	var runErrors []error
	for _, spec := range specs {
		fmt.Printf("START case=%s mode=%s subscriptions=%v\n", spec.ID, specMode(spec), runner.subscriptions)
		result := runner.runCase(spec, timeout)
		result.CaseFile = spec.ID + ".json"
		summary.Cases = append(summary.Cases, slim(result))
		if result.Error != "" {
			runErrors = append(runErrors, fmt.Errorf("%s: %s", spec.ID, result.Error))
		}
		if err := writeJSON(filepath.Join(outputDir, result.CaseFile), result); err != nil {
			return err
		}
		fmt.Printf("END case=%s selected=%d nodes=%d->%d subscriptions=%v duration=%s tokens=%d cache=%.2f%% peak_input=%d resets=%d assert_failures=%v degraded=%v error=%q\n",
			spec.ID,
			len(result.Selected),
			result.GraphNodesBefore,
			result.GraphNodesAfter,
			result.SubscriptionsAfter,
			result.Duration.Round(time.Millisecond),
			result.Metrics.MemoryOrganizerTokens,
			100*result.Metrics.TotalCacheHitRate,
			result.Discipline.PeakInputTokens,
			result.Discipline.SessionResets,
			result.AssertionFailures,
			result.Degradation,
			result.Error,
		)
		partial := summary
		partial.Duration = time.Since(summary.Started)
		partial.FinalSubscriptions = append([]string(nil), runner.subscriptions...)
		current := runner.view.Snapshot()
		partial.FinalGraphNodes = len(current.Nodes)
		partial.FinalGraphSubgraphs = len(current.Subgraphs)
		partial.Metrics = metricsFromEvents(runner.events.since(0))
		if err := writeJSON(filepath.Join(outputDir, "summary.partial.json"), partial); err != nil {
			return err
		}
	}
	finalGraph := runner.view.Snapshot()
	summary.Duration = time.Since(summary.Started)
	summary.FinalSubscriptions = append([]string(nil), runner.subscriptions...)
	summary.FinalGraphNodes = len(finalGraph.Nodes)
	summary.FinalGraphSubgraphs = len(finalGraph.Subgraphs)
	summary.Metrics = metricsFromEvents(runner.events.since(0))
	afterHash, err := fileSHA256(memoryPath)
	if err != nil {
		return err
	}
	summary.SourceUnmodified = beforeHash == afterHash
	if err := writeJSON(filepath.Join(outputDir, "summary.json"), summary); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outputDir, "final-graph.json"), finalGraph); err != nil {
		return err
	}
	// summary.json now holds everything the last partial did; keeping both doubles
	// the artifact for no added information.
	if err := os.Remove(filepath.Join(outputDir, "summary.partial.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove partial summary: %w", err)
	}
	if !summary.SourceUnmodified {
		runErrors = append(runErrors, errors.New("source memory.json changed during evaluation"))
	}
	return errors.Join(runErrors...)
}

// runOptions are the two product switches this harness A/Bs.
type runOptions struct {
	SessionReset bool
	Attribution  bool
}

func newEvalRunner(
	model agent.Provider,
	config provider.FileConfig,
	store *ctxgraph.Store,
	initial []string,
	options runOptions,
) (*evalRunner, error) {
	graph := store.Load(evalEnvID)
	known := make(map[string]struct{}, len(graph.Subgraphs))
	for _, subgraph := range graph.Subgraphs {
		known[subgraph.ID] = struct{}{}
	}
	for _, id := range initial {
		if _, ok := known[id]; !ok {
			return nil, fmt.Errorf("initial subscription %q does not exist", id)
		}
	}

	recorder := &eventRecorder{}
	bus := event.NewBus(recorder.handle)
	recording := &recordingProvider{inner: model}
	overlay := agent.FileOverlay{
		Tools:                   config.Tools,
		Prompts:                 config.Prompts,
		Events:                  bus,
		Curation:                config.Memory.Curation,
		NoSessionReset:          !options.SessionReset,
		SubscriptionAttribution: options.Attribution,
	}
	agents := agent.FileAgents{SubgraphOrganizer: config.Agents.SubgraphOrganizer}
	team, err := agent.NewTeam(
		recording,
		config.LLM.ContextWindow,
		agents,
		nil,
		overlay,
	)
	if err != nil {
		return nil, fmt.Errorf("create organizer: %w", err)
	}

	organizeTool := agent.OrganizeSubgraphTool(team.Organizer)
	requester, err := agent.NewLoop(agent.Config{
		AgentID:       "organizer-eval-requester",
		Provider:      recording,
		Tools:         []agenttool.Tool{organizeTool},
		ContextWindow: config.LLM.ContextWindow,
		Events:        bus,
	})
	if err != nil {
		return nil, fmt.Errorf("create requester: %w", err)
	}
	requester.SetSubscribedSubgraphs(initial)
	view := store.View(evalEnvID)
	workspace := env.Open(evalEnvID, view)
	if err := requester.Bind(workspace); err != nil {
		return nil, fmt.Errorf("bind organizer evaluation: %w", err)
	}
	bound := agenttool.BindEnv(workspace, []agenttool.Tool{organizeTool})
	if len(bound) != 1 {
		return nil, errors.New("bind organizer tool: expected one tool")
	}
	return &evalRunner{
		view:          view,
		tool:          bound[0],
		organizer:     team.Organizer,
		recording:     recording,
		events:        recorder,
		subscriptions: append([]string(nil), initial...),
		selected:      map[string][]ctxgraph.Node{},
	}, nil
}

func specMode(spec querySpec) string {
	if spec.Mode == "" {
		return modeQuery
	}
	return spec.Mode
}

func (r *evalRunner) runCase(spec querySpec, timeout time.Duration) caseResult {
	result := caseResult{Spec: spec, Started: time.Now()}
	before := r.view.Snapshot()
	result.GraphNodesBefore = len(before.Nodes)
	result.GraphSubgraphsBefore = len(before.Subgraphs)
	result.SubscriptionsBefore = append([]string(nil), r.subscriptions...)
	eventStart := r.events.count()
	exchangeStart := r.recording.count()

	arguments, err := json.Marshal(map[string]string{"query": spec.Query, "exclude": spec.Exclude})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var output agenttool.Output
	var execErr error
	if specMode(spec) == modeCurate {
		// The production trigger is a compaction threshold; the audit request and
		// the organizer's deep-curation mode are the same code either way.
		agent.RunDeepCuration(ctx, r.organizer, before)
	} else {
		output, execErr = r.tool.Execute(ctx, agenttool.Call{
			ID:        "organize-" + spec.ID,
			Name:      "organize_subgraph",
			Arguments: arguments,
		})
	}
	result.Duration = time.Since(result.Started)
	result.ToolOutput = output.Content
	result.Events = r.events.since(eventStart)
	result.Exchanges = r.recording.since(exchangeStart)
	result.Metrics = metricsFromEvents(result.Events)
	if execErr != nil {
		result.Error = execErr.Error()
	}
	var organized struct {
		Subgraph      ctxgraph.Subgraph `json:"subgraph"`
		Subscriptions struct {
			Subscribed   []string `json:"subscribed"`
			Unsubscribed []string `json:"unsubscribed"`
		} `json:"subscriptions"`
	}
	if output.Content != "" {
		if err := json.Unmarshal([]byte(output.Content), &organized); err != nil && result.Error == "" {
			result.Error = "decode organize_subgraph output: " + err.Error()
		}
	}
	result.Subgraph = organized.Subgraph
	if result.Subgraph.ID != "" {
		r.subscriptions = updateSubscriptions(
			r.subscriptions,
			organized.Subscriptions.Subscribed,
			organized.Subscriptions.Unsubscribed,
			result.Subgraph.ID,
		)
	}
	result.SubscriptionsAfter = append([]string(nil), r.subscriptions...)
	after := r.view.Snapshot()
	result.GraphNodesAfter = len(after.Nodes)
	result.GraphSubgraphsAfter = len(after.Subgraphs)
	result.GraphDelta = diffGraph(before, after)
	if result.Subgraph.ID != "" {
		result.Selected = after.NodesInSubgraphs([]string{result.Subgraph.ID})
	}
	r.selected[spec.ID] = result.Selected
	result.SharedWithEarlier = sharedSelection(result.Selected, r.selected[spec.Assert.MustShareWith])
	result.Discipline = measureDiscipline(after, result.Exchanges)
	result.Projection = measureProjection(after, result.SubscriptionsAfter)
	result.AssertionFailures = checkAssertions(
		spec,
		result.Selected,
		result.SubscriptionsAfter,
		result.SharedWithEarlier,
	)
	if specMode(spec) == modeQuery {
		result.Degradation = degradations(spec, result.Subgraph, result.Selected)
	}
	return result
}

func readQuerySpecs(path string) ([]querySpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries: %w", err)
	}
	var specs []querySpec
	if err := json.Unmarshal(data, &specs); err != nil {
		return nil, fmt.Errorf("decode queries: %w", err)
	}
	seen := make(map[string]struct{}, len(specs))
	for i := range specs {
		if specs[i].ID == "" || specs[i].Query == "" {
			return nil, fmt.Errorf("query %d: id and query are required", i)
		}
		if _, ok := seen[specs[i].ID]; ok {
			return nil, fmt.Errorf("duplicate query id %q", specs[i].ID)
		}
		if mode := specMode(specs[i]); mode != modeQuery && mode != modeCurate {
			return nil, fmt.Errorf("query %q: unknown mode %q", specs[i].ID, mode)
		}
		// A sharing assertion is only checkable against a case that already ran.
		if target := specs[i].Assert.MustShareWith; target != "" {
			if _, ok := seen[target]; !ok {
				return nil, fmt.Errorf("query %q: must_share_with %q must name an earlier query", specs[i].ID, target)
			}
		}
		seen[specs[i].ID] = struct{}{}
	}
	if len(specs) == 0 {
		return nil, errors.New("queries are empty")
	}
	return specs, nil
}

func newEvalStore(memoryPath, sourceEnv string) (*ctxgraph.Store, error) {
	source, err := ctxgraph.OpenStore(memoryPath)
	if err != nil {
		return nil, fmt.Errorf("open source memory: %w", err)
	}
	graph := source.Load(sourceEnv)
	if len(graph.Nodes) == 0 {
		return nil, fmt.Errorf("source environment %q has no memory nodes", sourceEnv)
	}
	store := ctxgraph.NewStore()
	if err := store.Save(evalEnvID, graph); err != nil {
		return nil, fmt.Errorf("clone source graph: %w", err)
	}
	return store, nil
}

func splitIDs(value string) []string {
	parts := strings.Split(value, ",")
	ids := make([]string, 0, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" || contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func updateSubscriptions(current, subscribe, unsubscribe []string, target string) []string {
	dropped := make(map[string]struct{}, len(unsubscribe))
	for _, id := range unsubscribe {
		dropped[id] = struct{}{}
	}
	next := make([]string, 0, len(current)+len(subscribe)+1)
	for _, id := range current {
		if _, remove := dropped[id]; !remove && !contains(next, id) {
			next = append(next, id)
		}
	}
	for _, id := range append(append([]string(nil), subscribe...), target) {
		if id != "" && !contains(next, id) {
			next = append(next, id)
		}
	}
	return next
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func metricsFromEvents(events []event.RuntimeEvent) event.MetricsSnapshot {
	collector := event.NewCollector()
	for _, ev := range events {
		collector.Handle(context.Background(), ev)
	}
	return collector.Snapshot()
}

func diffGraph(before, after ctxgraph.Graph) graphDelta {
	delta := graphDelta{RevisionBefore: before.Revision, RevisionAfter: after.Revision}
	beforeNodes := make(map[string]ctxgraph.Node, len(before.Nodes))
	afterNodes := make(map[string]ctxgraph.Node, len(after.Nodes))
	for _, node := range before.Nodes {
		beforeNodes[node.ID] = node
	}
	for _, node := range after.Nodes {
		afterNodes[node.ID] = node
		old, exists := beforeNodes[node.ID]
		if !exists {
			delta.NodesAdded = append(delta.NodesAdded, node)
		} else if !reflect.DeepEqual(old, node) {
			delta.NodesChanged = append(delta.NodesChanged, nodeChange{Before: old, After: node})
		}
	}
	for _, node := range before.Nodes {
		if _, exists := afterNodes[node.ID]; !exists {
			delta.NodesDeleted = append(delta.NodesDeleted, node)
		}
	}

	beforeSubgraphs := make(map[string]ctxgraph.Subgraph, len(before.Subgraphs))
	afterSubgraphs := make(map[string]ctxgraph.Subgraph, len(after.Subgraphs))
	for _, subgraph := range before.Subgraphs {
		beforeSubgraphs[subgraph.ID] = subgraph
	}
	for _, subgraph := range after.Subgraphs {
		afterSubgraphs[subgraph.ID] = subgraph
		old, exists := beforeSubgraphs[subgraph.ID]
		if !exists {
			delta.SubgraphsAdded = append(delta.SubgraphsAdded, subgraph)
		} else if !reflect.DeepEqual(old, subgraph) {
			delta.SubgraphsChanged = append(
				delta.SubgraphsChanged,
				subgraphChange{Before: old, After: subgraph},
			)
		}
	}
	for _, subgraph := range before.Subgraphs {
		if _, exists := afterSubgraphs[subgraph.ID]; !exists {
			delta.SubgraphsDeleted = append(delta.SubgraphsDeleted, subgraph)
		}
	}

	beforeEdges := make(map[ctxgraph.Edge]struct{}, len(before.Edges))
	afterEdges := make(map[ctxgraph.Edge]struct{}, len(after.Edges))
	for _, edge := range before.Edges {
		beforeEdges[edge] = struct{}{}
	}
	for _, edge := range after.Edges {
		afterEdges[edge] = struct{}{}
		if _, exists := beforeEdges[edge]; !exists {
			delta.EdgesAdded = append(delta.EdgesAdded, edge)
		}
	}
	for _, edge := range before.Edges {
		if _, exists := afterEdges[edge]; !exists {
			delta.EdgesDeleted = append(delta.EdgesDeleted, edge)
		}
	}
	return delta
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %q: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
