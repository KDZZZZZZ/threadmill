package coordination

import (
	"fmt"
	"testing"
)

func BenchmarkLegalHelpSources500Tasks(b *testing.B) {
	graph := newGraph()
	root := graph.AddTask()
	for range 499 {
		if _, err := graph.Spawn(root.Planner.ID, root.Verifier.ID); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	for b.Loop() {
		if sources := graph.legalHelpSources(root.Verifier.ID); len(sources) == 0 {
			b.Fatal("legal source set is empty")
		}
	}
}

func BenchmarkFormatHelpSources500Tasks(b *testing.B) {
	graph := newGraph()
	root := graph.AddTask()
	for range 499 {
		if _, err := graph.Spawn(root.Planner.ID, root.Verifier.ID); err != nil {
			b.Fatal(err)
		}
	}
	sources := graph.legalHelpSources(root.Verifier.ID)
	formatted := formatHelpSources(sources, func(string) bool { return false })

	b.ReportAllocs()
	for b.Loop() {
		if got := formatHelpSources(sources, func(string) bool { return false }); len(got) != len(formatted) {
			b.Fatalf("formatted length = %d, want %d", len(got), len(formatted))
		}
	}
	b.ReportMetric(float64(len(formatted)), "output-bytes")
}

func BenchmarkAddHelp500TaskGraph(b *testing.B) {
	base := newGraph()
	root := base.AddTask()
	for range 499 {
		if _, err := base.Spawn(root.Planner.ID, root.Verifier.ID); err != nil {
			b.Fatal(err)
		}
	}
	state := helpState{
		ID:     "help/benchmark",
		CallID: "benchmark",
		NodeID: root.Executor.ID,
		Reason: "benchmark",
	}
	base.helps = append(base.helps, state)

	for _, units := range []int{1, 10, 100, 500} {
		b.Run(fmt.Sprintf("units=%d", units), func(b *testing.B) {
			spawns := make([]PendingSpawn, units)
			for i := range spawns {
				spawns[i] = PendingSpawn{
					From: root.Planner.ID,
					Info: fmt.Sprintf("unit %d", i),
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				graph := cloneBenchmarkGraph(base)
				if _, _, err := graph.addHelp(state.ID, root.Executor.ID, spawns); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func cloneBenchmarkGraph(graph *Graph) *Graph {
	return &Graph{
		tasks:            append([]Task(nil), graph.tasks...),
		edges:            append([]Edge(nil), graph.edges...),
		nextID:           graph.nextID,
		helps:            cloneHelpStates(graph.helps),
		publishingTaskID: graph.publishingTaskID,
		publishedTaskID:  graph.publishedTaskID,
	}
}
