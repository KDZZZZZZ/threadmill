package context

import (
	"fmt"
	"testing"
)

func BenchmarkStoreStats10000Nodes(b *testing.B) {
	store := NewStore()
	nodes := make([]Node, 10000)
	for i := range nodes {
		nodes[i] = Node{ID: fmt.Sprintf("node-%d", i), Statement: "fact"}
	}
	if err := store.Save("env", Graph{Nodes: nodes}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = store.Stats()
	}
}

func BenchmarkGraphNodesInSubgraphs(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("nodes-%d", size), func(b *testing.B) {
			nodes := make([]Node, size)
			for i := range nodes {
				nodes[i] = Node{
					ID:          fmt.Sprintf("node-%d", i),
					Statement:   "fact",
					SubgraphIDs: []string{"selected"},
				}
			}
			graph := Graph{Nodes: nodes}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got := graph.NodesInSubgraphs([]string{"selected"})
				if len(got) != size {
					b.Fatalf("nodes = %d, want %d", len(got), size)
				}
			}
		})
	}
}
