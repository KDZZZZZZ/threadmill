package event

import (
	"context"
	"testing"
	"time"
)

func BenchmarkCollectorHandle(b *testing.B) {
	collector := NewCollector()
	ev := RuntimeEvent{
		Kind: KindTool, Phase: PhaseEnd, Duration: 25 * time.Millisecond,
	}
	b.ReportAllocs()
	for b.Loop() {
		collector.Handle(context.Background(), ev)
	}
}

func BenchmarkCollectorHandleParallel(b *testing.B) {
	collector := NewCollector()
	ev := RuntimeEvent{
		Kind: KindTool, Phase: PhaseEnd, Duration: 25 * time.Millisecond,
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			collector.Handle(context.Background(), ev)
		}
	})
}

func BenchmarkCollectorSnapshot(b *testing.B) {
	collector := NewCollector()
	for range 1000 {
		collector.Handle(context.Background(), RuntimeEvent{
			Kind: KindModel, Phase: PhaseEnd, Duration: 250 * time.Millisecond,
			Tokens: 100,
		})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = collector.Snapshot()
	}
}
