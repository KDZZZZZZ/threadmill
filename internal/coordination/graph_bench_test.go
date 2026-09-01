package coordination

import "testing"

func BenchmarkSnapshot500Tasks(b *testing.B) {
	graph := newGraph()
	root := graph.AddTask()
	for range 499 {
		if _, err := graph.Spawn(root.Planner.ID, root.Verifier.ID); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	for b.Loop() {
		if snapshot := graph.Snapshot(); len(snapshot.Tasks) != 500 {
			b.Fatalf("tasks = %d, want 500", len(snapshot.Tasks))
		}
	}
}
