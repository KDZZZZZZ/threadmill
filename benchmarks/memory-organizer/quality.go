package main

import (
	"encoding/json"
	"strconv"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

// assertions are per-query, machine-checkable expectations. They never abort a
// run: a failed assertion is reported next to the case so one artifact set can
// be replayed against a changed expectation without re-spending model calls.
type assertions struct {
	MustInclude        []string `json:"must_include,omitempty"`
	MustExclude        []string `json:"must_exclude,omitempty"`
	MinSelected        *int     `json:"expect_min_selected,omitempty"`
	MaxSelected        *int     `json:"expect_max_selected,omitempty"`
	MustStaySubscribed []string `json:"must_stay_subscribed,omitempty"`
	// MustShareWith names an earlier case whose selection must overlap this one.
	// Subgraphs are meant to be semantically atomic, not orthogonal: a node that
	// answers two questions belongs to both subgraphs.
	MustShareWith string `json:"must_share_with,omitempty"`
}

// checkAssertions returns one line per unmet expectation.
func checkAssertions(spec querySpec, selected []ctxgraph.Node, subscriptions []string, shared int) []string {
	ids := make(map[string]struct{}, len(selected))
	for _, node := range selected {
		ids[node.ID] = struct{}{}
	}
	var failures []string
	for _, id := range spec.Assert.MustInclude {
		if _, ok := ids[id]; !ok {
			failures = append(failures, "must_include "+id)
		}
	}
	for _, id := range spec.Assert.MustExclude {
		if _, ok := ids[id]; ok {
			failures = append(failures, "must_exclude "+id)
		}
	}
	if spec.Assert.MinSelected != nil && len(selected) < *spec.Assert.MinSelected {
		failures = append(failures, "expect_min_selected "+strconv.Itoa(*spec.Assert.MinSelected)+
			" got "+strconv.Itoa(len(selected)))
	}
	if spec.Assert.MaxSelected != nil && len(selected) > *spec.Assert.MaxSelected {
		failures = append(failures, "expect_max_selected "+strconv.Itoa(*spec.Assert.MaxSelected)+
			" got "+strconv.Itoa(len(selected)))
	}
	for _, id := range spec.Assert.MustStaySubscribed {
		if !contains(subscriptions, id) {
			failures = append(failures, "must_stay_subscribed "+id)
		}
	}
	if spec.Assert.MustShareWith != "" && shared == 0 {
		failures = append(failures, "must_share_with "+spec.Assert.MustShareWith)
	}
	return failures
}

// degradations flags the shapes a cold or confused model produces while still
// returning a well-formed subgraph. They are observations, not run failures.
func degradations(spec querySpec, subgraph ctxgraph.Subgraph, selected []ctxgraph.Node) []string {
	var flags []string
	if len(selected) == 0 {
		flags = append(flags, "selected=0")
	}
	if subgraph.Admission == "" || subgraph.Scope == "" {
		flags = append(flags, "missing admission/scope")
	}
	if subgraph.Summary == spec.Query {
		flags = append(flags, "summary copies the query")
	}
	if subgraph.Revision == 0 {
		flags = append(flags, "subgraph revision=0")
	}
	return flags
}

// disciplineMetrics turns the recorded tool calls into the expansion discipline
// the prompt asks for: level 1 → 2 → 3 before committing membership, navigation
// tools for frontier completion, collapse to pay the context back.
type disciplineMetrics struct {
	MembershipCommits          int `json:"membership_commits"`
	MembershipCommitsWithoutL3 int `json:"membership_commits_without_l3"`
	NavigationCalls            int `json:"navigation_calls"`
	ExpandCalls                int `json:"expand_calls"`
	CollapseCalls              int `json:"collapse_calls"`
	DropCalls                  int `json:"drop_calls"`
	SessionResets              int `json:"session_resets"`
	PeakInputTokens            int `json:"peak_input_tokens"`
}

var navigationTools = map[string]struct{}{
	"memory_neighbors":    {},
	"memory_subgraphs_of": {},
	"memory_sources_of":   {},
	"memory_nodes_in":     {},
}

// levelTracker replays the view levels the organizer set on itself, mirroring
// internal/agent/memory_view.go: an explicit node level wins, otherwise the
// highest level of any subgraph holding the node, otherwise the default.
type levelTracker struct {
	graph   ctxgraph.Graph
	def     int
	targets map[string]int
}

func newLevelTracker(graph ctxgraph.Graph) *levelTracker {
	return &levelTracker{graph: graph, def: 1, targets: map[string]int{}}
}

func (t *levelTracker) expand(targets []string, level int) {
	if len(targets) == 0 {
		t.def = level
		return
	}
	for _, id := range targets {
		t.targets[id] = level
	}
}

func (t *levelTracker) collapse(targets []string, level int) {
	if len(targets) == 0 {
		t.def = level
		for id, existing := range t.targets {
			if existing > level {
				delete(t.targets, id)
			}
		}
		return
	}
	for _, id := range targets {
		t.targets[id] = level
	}
}

func (t *levelTracker) nodeLevel(id string) int {
	if level, ok := t.targets[id]; ok {
		return level
	}
	best := 0
	for _, node := range t.graph.Nodes {
		if node.ID != id {
			continue
		}
		for _, subgraph := range node.SubgraphIDs {
			if level, ok := t.targets[subgraph]; ok && level > best {
				best = level
			}
		}
	}
	if best > 0 {
		return best
	}
	return t.def
}

func measureDiscipline(graph ctxgraph.Graph, exchanges []exchange) disciplineMetrics {
	metrics := disciplineMetrics{}
	tracker := newLevelTracker(graph)
	previousMessages := -1
	for _, item := range exchanges {
		count := len(item.Request.Messages)
		if previousMessages >= 0 && count < previousMessages {
			metrics.SessionResets++
		}
		previousMessages = count
		if item.Response.Usage != nil && item.Response.Usage.InputTokens > metrics.PeakInputTokens {
			metrics.PeakInputTokens = item.Response.Usage.InputTokens
		}
		for _, call := range item.Response.ToolCalls {
			if _, ok := navigationTools[call.Name]; ok {
				metrics.NavigationCalls++
				continue
			}
			switch call.Name {
			case "memory_expand":
				metrics.ExpandCalls++
				targets, level := viewArgs(call.Arguments, 3)
				tracker.expand(targets, level)
			case "memory_collapse":
				metrics.CollapseCalls++
				targets, level := viewArgs(call.Arguments, 1)
				tracker.collapse(targets, level)
			case "memory_drop_from_context":
				metrics.DropCalls++
			case "memory_add_to_subgraph":
				for _, id := range addToSubgraphNodes(call.Arguments) {
					metrics.MembershipCommits++
					if tracker.nodeLevel(id) < 3 {
						metrics.MembershipCommitsWithoutL3++
					}
				}
			case "memory_apply":
				for _, id := range attachedNodes(call.Arguments) {
					metrics.MembershipCommits++
					if tracker.nodeLevel(id) < 3 {
						metrics.MembershipCommitsWithoutL3++
					}
				}
			}
		}
	}
	return metrics
}

func viewArgs(raw json.RawMessage, fallback int) ([]string, int) {
	var args struct {
		Targets []string `json:"targets"`
		Level   *int     `json:"level"`
	}
	_ = json.Unmarshal(raw, &args)
	level := fallback
	if args.Level != nil {
		level = *args.Level
	}
	return args.Targets, level
}

func addToSubgraphNodes(raw json.RawMessage) []string {
	var args struct {
		NodeIDs []string `json:"node_ids"`
	}
	_ = json.Unmarshal(raw, &args)
	return args.NodeIDs
}

func attachedNodes(raw json.RawMessage) []string {
	var args struct {
		Ops []struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		} `json:"ops"`
	}
	_ = json.Unmarshal(raw, &args)
	ids := make([]string, 0, len(args.Ops))
	for _, op := range args.Ops {
		if op.Action == "attach" && op.ID != "" {
			ids = append(ids, op.ID)
		}
	}
	return ids
}

// projectionCost sizes the subscriber-side injection block under both
// renderings, so the attribution A/B has token data before it picks a default.
type projectionCost struct {
	FlatBytes    int `json:"flat_bytes"`
	GroupedBytes int `json:"grouped_bytes"`
}

func measureProjection(graph ctxgraph.Graph, subscriptions []string) projectionCost {
	return projectionCost{
		FlatBytes:    len(agent.FormatSubscribedMemory(graph, subscriptions, false)),
		GroupedBytes: len(agent.FormatSubscribedMemory(graph, subscriptions, true)),
	}
}

// sharedSelection counts nodes this case selected that an earlier case also
// selected: the multi-membership signal the orthogonality question turns on.
func sharedSelection(selected []ctxgraph.Node, earlier []ctxgraph.Node) int {
	seen := make(map[string]struct{}, len(earlier))
	for _, node := range earlier {
		seen[node.ID] = struct{}{}
	}
	shared := 0
	for _, node := range selected {
		if _, ok := seen[node.ID]; ok {
			shared++
		}
	}
	return shared
}
