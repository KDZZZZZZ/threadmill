package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestBindMemoryToolsStayInTheirEnv(t *testing.T) {
	t.Parallel()

	store := ctxgraph.NewStore()
	graph := ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "secret",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	}
	store.Save("env-a", graph)
	store.Save("env-b", graph)

	toolsA := Bind(store, "env-a", MemoryTools(nil, nil))
	toolsB := Bind(store, "env-b", MemoryTools(nil, nil))

	if _, err := executeNamed(t, toolsA, memoryAddToSubgraphName, `{"subgraph_id":"secret","node_ids":["n1"]}`); err != nil {
		t.Fatalf("env-a add: %v", err)
	}

	outB, err := executeNamed(t, toolsB, memoryNodesInName, `{"subgraph_ids":["secret"]}`)
	if err != nil {
		t.Fatalf("env-b nodes_in: %v", err)
	}
	if strings.Contains(outB.Content, `"n1"`) {
		t.Fatalf("env-b saw env-a write: %s", outB.Content)
	}

	outA, err := executeNamed(t, toolsA, memoryNodesInName, `{"subgraph_ids":["secret"]}`)
	if err != nil {
		t.Fatalf("env-a nodes_in: %v", err)
	}
	if !strings.Contains(outA.Content, `"n1"`) {
		t.Fatalf("env-a did not see its write: %s", outA.Content)
	}
}

func TestBindReplacesGlobalMemoryTools(t *testing.T) {
	t.Cleanup(func() { ctxgraph.Update(ctxgraph.Copy{}) })
	ctxgraph.Update(ctxgraph.Copy{})

	store := ctxgraph.NewStore()
	store.Save("env-1", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{
			ID:          "n1",
			Kind:        ctxgraph.NodeKindFact,
			Statement:   "local",
			Status:      ctxgraph.NodeStatusAccepted,
			SubgraphIDs: []string{"sg"},
		}},
	})

	leaking := MemoryTools(func() ctxgraph.Copy {
		return ctxgraph.Clone("leak")
	}, ctxgraph.Update)
	tools := Bind(store, "env-1", leaking)

	if _, err := executeNamed(t, tools, memoryAddToSubgraphName, `{"subgraph_id":"bound","node_ids":["n1"]}`); err != nil {
		t.Fatalf("add: %v", err)
	}

	if nodes := ctxgraph.Clone("check").Graph.NodesInSubgraphs([]string{"bound"}); len(nodes) != 0 {
		t.Fatalf("bound write leaked to global graph: %#v", nodes)
	}
	if nodes := store.Load("env-1").NodesInSubgraphs([]string{"bound"}); len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatal("bound write did not stay in env-1")
	}
}

func TestBindInjectsEnvIntoToolContext(t *testing.T) {
	t.Parallel()

	var seen string
	spy := spyTool{
		name: "echo",
		execute: func(ctx context.Context, _ Call) (Output, error) {
			seen = EnvFromContext(ctx)
			return Output{Content: "ok"}, nil
		},
	}

	tools := Bind(ctxgraph.NewStore(), "env-9", []Tool{spy})
	if _, err := executeNamed(t, tools, "echo", `{}`); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if seen != "env-9" {
		t.Fatalf("EnvFromContext = %q, want env-9", seen)
	}
}

type spyTool struct {
	name    string
	execute func(context.Context, Call) (Output, error)
}

func (s spyTool) Definition() Definition {
	return Definition{
		Name:        s.name,
		Description: "spy",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (s spyTool) Execute(ctx context.Context, call Call) (Output, error) {
	return s.execute(ctx, call)
}

func executeNamed(t *testing.T, tools []Tool, name, arguments string) (Output, error) {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition().Name != name {
			continue
		}
		return tool.Execute(context.Background(), Call{
			ID:        "call-1",
			Name:      name,
			Arguments: json.RawMessage(arguments),
		})
	}
	t.Fatalf("tool %q not registered", name)
	return Output{}, nil
}
