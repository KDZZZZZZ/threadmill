package coordination

import "testing"

func BenchmarkSnapshot500Tasks(b *testing.B) {
	graph := New()
	source := graph.AddTask()
	for range 499 {
		consumer := graph.AddTask()
		if err := graph.Connect(source.Planner.ID, consumer.Planner.ID); err != nil {
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
