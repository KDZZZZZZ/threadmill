package exec

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestCostStatsAreBounded(t *testing.T) {
	s := New(Config{Slots: 1, ExternalSandbox: true})
	for i := range maxCostEntries + 1 {
		s.costs.record(fmt.Sprintf("command-%d", i), time.Second, 1024)
	}

	stats := s.CostStats()
	if len(stats) != maxCostEntries {
		t.Fatalf("cost entries = %d, want %d", len(stats), maxCostEntries)
	}
	if _, ok := stats["command-0"]; ok {
		t.Fatal("oldest cost entry was not evicted")
	}
	if _, ok := stats[fmt.Sprintf("command-%d", maxCostEntries)]; !ok {
		t.Fatal("newest cost entry was not retained")
	}
}

func TestCollectMeasuresPeakRSS(t *testing.T) {
	if testing.Short() {
		t.Skip("peak RSS sampling needs real processes")
	}
	// 让子 shell 分配 ~24MB 并停留 400ms，验证采样会沿命令进程树向下统计。
	cmd := "bash -c 'data=$(head -c 24M /dev/zero | base64); sleep 0.4' & wait"
	result, err := runExternalSandbox(context.Background(), t.TempDir(), t.TempDir(), cmd, 1<<20, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.PeakRSSBytes < 8<<20 {
		t.Errorf("PeakRSSBytes = %d, want > 8MB", result.PeakRSSBytes)
	}
}

func TestSchedulerRecordsCommandCost(t *testing.T) {
	store, err := vfs.NewPersistentStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Slots: 1, Timeout: 10 * time.Second})
	s.run = func(ctx context.Context, live string, spec env.Cmd) (env.ExecResult, error) {
		return env.ExecResult{ExitCode: 0, PeakRSSBytes: 4096}, nil
	}
	view := s.View("env-cost", store)
	if _, err := view.Run(context.Background(), env.Cmd{Command: "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	stats := s.CostStats()
	stat, ok := stats["go test ./..."]
	if !ok {
		t.Fatalf("cost stats = %v, want entry for command", stats)
	}
	if stat.Count != 1 || stat.PeakRSS != 4096 {
		t.Errorf("stat = %+v, want count 1 peak 4096", stat)
	}
}
