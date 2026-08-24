package exec

import (
	"context"
	"strings"
	"time"
)

const (
	defaultHeavyThreshold = 10 * time.Second
	defaultMemPoll        = 5 * time.Millisecond
)

var coldHeavyMarkers = [...]string{
	"cargo build", "cargo check", "cargo clippy", "cargo test",
	"go build", "go generate", "go test", "go vet",
	"npm run build", "npm test", "pnpm build", "pnpm test",
	"pytest", "python -m pytest",
}

// commandClass 依据成本表把命令分到重/轻车道；常见构建和测试命令冷启动时保守进入重车道。
func (s *Scheduler) commandClass(command string) (heavy bool, peakRSS uint64) {
	stat, ok := s.costs.lookup(command)
	if !ok || stat.Count == 0 {
		lower := strings.ToLower(command)
		for _, marker := range coldHeavyMarkers {
			if strings.Contains(lower, marker) {
				return true, 0
			}
		}
		return false, 0
	}
	avg := stat.Duration / time.Duration(stat.Count)
	return avg >= s.heavyThreshold, stat.PeakRSS
}

// reserveMemory 等到预算能容纳 est 再返回；est 未知（0）时放行。
func (s *Scheduler) reserveMemory(ctx context.Context, est uint64) bool {
	if s.memBudget <= 0 || est == 0 {
		return true
	}
	for {
		if err := ctx.Err(); err != nil {
			return false
		}
		s.memMu.Lock()
		if s.memInUse+int64(est) <= s.memBudget {
			s.memInUse += int64(est)
			s.memMu.Unlock()
			return true
		}
		s.memMu.Unlock()
		time.Sleep(defaultMemPoll)
	}
}

func (s *Scheduler) releaseMemory(est uint64) {
	if s.memBudget <= 0 || est == 0 {
		return
	}
	s.memMu.Lock()
	s.memInUse -= int64(est)
	if s.memInUse < 0 {
		s.memInUse = 0
	}
	s.memMu.Unlock()
}
