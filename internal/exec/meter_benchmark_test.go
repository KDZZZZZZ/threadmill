package exec

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func BenchmarkProcMeterSample(b *testing.B) {
	meter := newProcMeter()
	pid := os.Getpid()
	b.ReportAllocs()
	for b.Loop() {
		meter.sampleOnce(pid)
	}
}

func BenchmarkCommandClassWithCostHistory(b *testing.B) {
	scheduler := New(Config{Slots: 1})
	for i := range 4096 {
		scheduler.costs.record(fmt.Sprintf("command-%d", i), time.Second, 1024)
	}
	scheduler.costs.record("go test ./...", 15*time.Second, 2048)
	b.ReportAllocs()
	for b.Loop() {
		scheduler.commandClass("go test ./...")
	}
}
