package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func TestOrganizeInputMemoryIdenticalSourcesBypassModel(t *testing.T) {
	t.Parallel()
	graph := ctxgraph.Graph{Nodes: []ctxgraph.Node{{ID: "n", Statement: "unchanged"}}}
	for _, sourceCount := range []int{1, 2} {
		sources := make([]ctxgraph.InputSource, sourceCount)
		for i := range sources {
			sources[i] = ctxgraph.InputSource{Ref: "output", Graph: graph}
		}
		result, err := OrganizeInputMemory(context.Background(), Config{}, InputMemoryRequest{Sources: sources})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Nodes, graph.Nodes) {
			t.Fatalf("result = %#v, want unchanged single/all-identical input", result)
		}
		result.Nodes[0].Statement = "later target edit"
		if graph.Nodes[0].Statement != "unchanged" {
			t.Fatal("result mutates its source")
		}
	}
}

func TestOrganizeInputMemoryRejectsIncompleteSourceBeforeBypass(t *testing.T) {
	t.Parallel()
	_, err := OrganizeInputMemory(context.Background(), Config{}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{{Ref: "source", Graph: ctxgraph.Graph{
			Nodes: []ctxgraph.Node{{ID: "n", Statement: "broken membership", SubgraphIDs: []string{"missing"}}},
		}}},
	})
	if err == nil {
		t.Fatal("single-source bypass accepted dangling memory references")
	}
}

func TestOrganizeInputMemoryUsesFinalFilesAndOnlyDifferences(t *testing.T) {
	t.Parallel()
	common := ctxgraph.Node{
		ID: "unrelated", Statement: "COMMON_MEMORY_MUST_NOT_BE_ORGANIZED",
		Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted,
		SubgraphIDs: []string{"knowledge"},
	}
	first := ctxgraph.Graph{
		Subgraphs: []ctxgraph.Subgraph{{ID: "knowledge"}},
		Nodes: []ctxgraph.Node{common, {
			ID: "cache", Statement: "cache is enabled", Kind: ctxgraph.NodeKindFact,
			Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"knowledge"},
		}},
	}
	second := first.Clone()
	second.Nodes = second.Nodes[:1]
	calls := 0
	provider := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			if len(request.Messages) != 1 || !strings.Contains(request.Messages[0].Content, "cache.go is absent") {
				t.Fatalf("organizer did not receive final file evidence in a fresh session: %#v", request.Messages)
			}
			if strings.Contains(request.Messages[0].Content, common.Statement) {
				t.Fatal("unrelated common memory was included in organization input")
			}
			for _, definition := range request.Tools {
				if !strings.HasPrefix(definition.Name, "memory_") {
					t.Fatalf("organizer received non-memory capability %q", definition.Name)
				}
			}
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID: "reject-file-fact", Name: "memory_apply",
				Arguments: json.RawMessage(`{"ops":[{"action":"update","id":"cache","statement":"Source A enabled cache; the final files omit it","status":"outdated","reason":"files-final: cache.go is absent"}]}`),
			}}}, nil
		}
		return AssistantMessage{Content: "file-dependent claim retained as history"}, nil
	})
	result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources:  []ctxgraph.InputSource{{Ref: "output-a", Graph: first}, {Ref: "output-b", Graph: second}},
		FilesRef: "files-final", Evidence: "cache.go is absent in final files",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(result.Nodes) != 2 {
		t.Fatalf("organizer calls = %d, result = %#v", calls, result)
	}
	if !reflect.DeepEqual(result.Nodes[0], common) {
		t.Fatal("common memory changed")
	}
	if result.Nodes[1].Status != ctxgraph.NodeStatusOutdated || !strings.Contains(result.Nodes[1].Statement, "final files omit") {
		t.Fatalf("rejected file claim was not qualified: %#v", result.Nodes[1])
	}
	if first.Nodes[1].Status != ctxgraph.NodeStatusAccepted || first.Nodes[1].Statement != "cache is enabled" {
		t.Fatal("organizer mutated source memory")
	}
}

func TestOrganizeInputMemoryUnsupportedFactsStayDisputed(t *testing.T) {
	t.Parallel()
	calls := 0
	provider := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID: "unsupported-acceptance", Name: "memory_apply",
				Arguments: json.RawMessage(`{"ops":[{"action":"status","id":"claim","status":"accepted","reason":"source said it worked"}]}`),
			}}}, nil
		}
		return AssistantMessage{Content: "no final file evidence; keep the claim disputed"}, nil
	})
	result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{
			{Ref: "source", Graph: ctxgraph.Graph{Nodes: []ctxgraph.Node{{
				ID: "claim", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "tests pass",
			}}}},
			{Ref: "other"},
		},
		FilesRef: "files-final",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Status != ctxgraph.NodeStatusDisputed {
		t.Fatalf("unsupported candidate became current fact: %#v", result.Nodes)
	}
	if !reflect.DeepEqual(result.Nodes[0].SourceRefs, []string{"source"}) {
		t.Fatalf("qualified fact lost its source: %#v", result.Nodes[0])
	}
}

func TestOrganizeInputMemoryAcceptedFactsKeepTheirFinalFileReference(t *testing.T) {
	t.Parallel()
	calls := 0
	provider := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID: "verify", Name: "memory_apply",
				Arguments: json.RawMessage(`{"ops":[{"action":"status","id":"claim","status":"accepted","reason":"files-final: cache enabled in actual config"}]}`),
			}}}, nil
		}
		return AssistantMessage{Content: "accepted from final evidence"}, nil
	})
	result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{
			{Ref: "source", Graph: ctxgraph.Graph{Nodes: []ctxgraph.Node{{
				ID: "claim", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "cache enabled",
			}}}}, {Ref: "other"},
		},
		FilesRef: "files-final", Evidence: "config.yaml: cache.enabled: true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Status != ctxgraph.NodeStatusAccepted ||
		!reflect.DeepEqual(result.Nodes[0].SourceRefs, []string{"source", "files-final"}) {
		t.Fatalf("accepted fact is not bound to its evidence: %#v", result.Nodes)
	}
}

func TestOrganizeInputMemoryCannotBypassEvidenceByChangingKind(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		kind      string
		operation string
	}{
		{name: "ignored kind in status operation", kind: ctxgraph.NodeKindFact,
			operation: `{"action":"status","id":"claim","status":"accepted","kind":"hypothesis","reason":"trust source"}`},
		{name: "hypothesis promoted without status", kind: ctxgraph.NodeKindHypothesis,
			operation: `{"action":"update","id":"claim","kind":"fact","statement":"implementation exists","reason":"trust source"}`},
		{name: "fact disguised as directive", kind: ctxgraph.NodeKindFact,
			operation: `{"action":"update","id":"claim","kind":"directive","status":"accepted","statement":"implementation exists","reason":"trust source"}`},
		{name: "kind changed earlier in batch", kind: ctxgraph.NodeKindHypothesis,
			operation: `{"action":"update","id":"claim","kind":"fact","status":"disputed","statement":"implementation exists","reason":"needs verification"},{"action":"status","id":"claim","status":"accepted","reason":"trust source"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			provider := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
				calls++
				if calls == 1 {
					return AssistantMessage{ToolCalls: []agenttool.Call{{
						ID: "bypass", Name: "memory_apply",
						Arguments: json.RawMessage(`{"ops":[` + test.operation + `]}`),
					}}}, nil
				}
				return AssistantMessage{Content: "no supported change"}, nil
			})
			result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
				Sources: []ctxgraph.InputSource{
					{Ref: "source", Graph: ctxgraph.Graph{Nodes: []ctxgraph.Node{{
						ID: "claim", Kind: test.kind, Status: ctxgraph.NodeStatusAccepted, Statement: "implementation exists",
					}}}}, {Ref: "other"},
				},
				FilesRef: "files-final", Evidence: "implementation state was not established",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Nodes) != 1 || result.Nodes[0].Kind != test.kind ||
				(test.kind == ctxgraph.NodeKindFact && result.Nodes[0].Status != ctxgraph.NodeStatusDisputed) {
				t.Fatalf("unsupported fact bypassed input validation: %#v", result.Nodes)
			}
		})
	}
}

func TestOrganizeInputMemoryFailureDoesNotLeakDraftOrCallerHistory(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"provider failure", "cancel", "success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checkpoints, err := NewDirCheckpointStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := Checkpoint{Messages: []Message{{Role: "user", Content: "CALLER_HISTORY", ModelData: json.RawMessage(`null`)}}}
			if err := checkpoints.Save("organizer", checkpoint); err != nil {
				t.Fatal(err)
			}
			source := ctxgraph.Graph{Nodes: []ctxgraph.Node{{
				ID: "claim", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "source fact",
			}}}
			failure := errors.New("provider failed after draft edit")
			calls := 0
			provider := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
				calls++
				for _, message := range request.Messages {
					if strings.Contains(message.Content, "CALLER_HISTORY") {
						t.Fatal("input organizer restored the caller's raw history")
					}
				}
				if calls == 1 {
					return AssistantMessage{ToolCalls: []agenttool.Call{{
						ID: "draft-write", Name: "memory_apply",
						Arguments: json.RawMessage(`{"ops":[{"action":"update","id":"claim","statement":"DRAFT_ONLY","status":"disputed","reason":"needs verification"}]}`),
					}}}, nil
				}
				if mode == "provider failure" {
					return AssistantMessage{}, failure
				}
				if mode == "cancel" {
					cancel()
				}
				return AssistantMessage{Content: "finished draft"}, nil
			})
			result, err := OrganizeInputMemory(ctx, Config{
				AgentID: "organizer", Provider: provider, CheckpointStore: checkpoints,
				Hooks: Hooks{CommitTurn: []CommitTurnHook{func(context.Context) error {
					t.Fatal("input organizer invoked caller memory writeback hook")
					return nil
				}}},
			}, InputMemoryRequest{
				Sources:  []ctxgraph.InputSource{{Ref: "source", Graph: source}, {Ref: "other"}},
				FilesRef: "files-final",
			})
			if mode == "success" {
				if err != nil || len(result.Nodes) != 1 || result.Nodes[0].Statement != "DRAFT_ONLY" {
					t.Fatalf("successful draft = %#v, error = %v", result, err)
				}
			} else {
				want := failure
				if mode == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || len(result.Nodes) != 0 {
					t.Fatalf("failed organizer leaked a result: %#v, error = %v", result, err)
				}
			}
			if source.Nodes[0].Statement != "source fact" || source.Nodes[0].Status != ctxgraph.NodeStatusAccepted {
				t.Fatal("source memory changed")
			}
			stored, found, err := checkpoints.Load("organizer")
			if err != nil || !found || !reflect.DeepEqual(stored, checkpoint) {
				t.Fatalf("caller checkpoint changed: %#v, found=%t, error=%v", stored, found, err)
			}
		})
	}
}

func TestOrganizeInputMemoryRejectsUnboundedModelEdits(t *testing.T) {
	t.Parallel()
	arguments, err := json.Marshal(map[string]any{"ops": []map[string]string{{
		"action": "update", "id": "claim", "statement": strings.Repeat("x", 64<<10),
		"status": "disputed", "reason": "unsupported draft",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{ID: "oversized", Name: "memory_apply", Arguments: arguments}}}, nil
		}
		return AssistantMessage{Content: "leave the bounded original"}, nil
	})
	result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{
			{Ref: "source", Graph: ctxgraph.Graph{Nodes: []ctxgraph.Node{{
				ID: "claim", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "original",
			}}}}, {Ref: "other"},
		}, FilesRef: "files-final",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Statement != "original" {
		t.Fatal("unbounded model output reached the memory graph")
	}
}

func TestOrganizeInputMemoryNewConclusionsRetainInputProvenance(t *testing.T) {
	t.Parallel()
	calls := 0
	provider := modelFunc(func(context.Context, Request) (AssistantMessage, error) {
		calls++
		if calls == 1 {
			return AssistantMessage{ToolCalls: []agenttool.Call{{
				ID: "qualify", Name: "memory_apply",
				Arguments: json.RawMessage(`{"ops":[{"action":"create","kind":"hypothesis","statement":"cache may need verification","reason":"input sources disagree"}]}`),
			}}}, nil
		}
		return AssistantMessage{Content: "kept a qualified conclusion"}, nil
	})
	result, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{
			{Ref: "source-a", Graph: ctxgraph.Graph{Nodes: []ctxgraph.Node{{
				ID: "claim", Kind: ctxgraph.NodeKindFact, Status: ctxgraph.NodeStatusAccepted, Statement: "cache exists",
			}}}}, {Ref: "source-b"},
		}, FilesRef: "files-final",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 2 || !reflect.DeepEqual(result.Nodes[1].SourceRefs, []string{"source-a", "source-b"}) {
		t.Fatalf("new conclusion lost its source batch: %#v", result.Nodes)
	}
}

func TestOrganizeInputMemoryRetainsSubgraphDifferenceProvenance(t *testing.T) {
	t.Parallel()
	provider := modelFunc(func(_ context.Context, request Request) (AssistantMessage, error) {
		prompt := request.Messages[0].Content
		for _, source := range []string{"source-a", "source-b"} {
			if !strings.Contains(prompt, source) {
				t.Fatalf("organizer cannot attribute a subgraph variant to %s", source)
			}
		}
		return AssistantMessage{Content: "keep the distinct scopes"}, nil
	})
	_, err := OrganizeInputMemory(context.Background(), Config{Provider: provider}, InputMemoryRequest{
		Sources: []ctxgraph.InputSource{
			{Ref: "source-a", Graph: ctxgraph.Graph{Subgraphs: []ctxgraph.Subgraph{{ID: "knowledge", Scope: "before"}}}},
			{Ref: "source-b", Graph: ctxgraph.Graph{Subgraphs: []ctxgraph.Subgraph{{ID: "knowledge", Scope: "after"}}}},
		}, FilesRef: "files-final",
	})
	if err != nil {
		t.Fatal(err)
	}
}
