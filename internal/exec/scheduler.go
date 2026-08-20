// Package exec 按槽位调度在隔离 live 目录里跑的命令。
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

var ErrSandboxUnavailable = errors.New("exec: SANDBOX_UNAVAILABLE")

const defaultOutputCapKB = 256

type sandboxKind int

const (
	sandboxNone sandboxKind = iota
	sandboxBwrap
)

// Config 是执行调度器的槽位、超时和输出上限。
type Config struct {
	Slots       int
	Timeout     time.Duration
	OutputCapKB int
}

// Scheduler 用信号量限制并发，并把命令跑进某个 env 的 live 目录。
type Scheduler struct {
	slots     chan struct{}
	capacity  int
	timeout   time.Duration
	outputCap int
	sandbox   sandboxKind
	run       func(context.Context, string, env.Cmd) (env.ExecResult, error)

	mu       sync.Mutex
	groups   map[string][]int
	counters schedulerCounters
}

type schedulerCounters struct {
	requests     uint64
	started      uint64
	completed    uint64
	errors       uint64
	canceled     uint64
	timedOut     uint64
	queued       int
	active       int
	peakQueued   int
	peakActive   int
	waitDuration time.Duration
	runDuration  time.Duration
}

// Stats 是执行槽位、排队和完成情况的并发一致快照。
type Stats struct {
	Capacity             int           `json:"capacity"`
	Queued               int           `json:"queued"`
	Active               int           `json:"active"`
	PeakQueued           int           `json:"peak_queued"`
	PeakActive           int           `json:"peak_active"`
	Requests             uint64        `json:"requests"`
	Started              uint64        `json:"started"`
	Completed            uint64        `json:"completed"`
	Errors               uint64        `json:"errors"`
	Canceled             uint64        `json:"canceled"`
	TimedOut             uint64        `json:"timed_out"`
	WaitDuration         time.Duration `json:"wait_duration"`
	RunDuration          time.Duration `json:"run_duration"`
	TrackedProcessGroups int           `json:"tracked_process_groups"`
}

// New 创建调度器。Slots <= 0 时用 runtime.NumCPU()。
func New(cfg Config) *Scheduler {
	n := cfg.Slots
	if n <= 0 {
		n = runtime.NumCPU()
	}
	capBytes := cfg.OutputCapKB * 1024
	if capBytes <= 0 {
		capBytes = defaultOutputCapKB * 1024
	}
	s := &Scheduler{
		slots:     make(chan struct{}, n),
		capacity:  n,
		timeout:   cfg.Timeout,
		outputCap: capBytes,
		sandbox:   probeSandbox(),
	}
	for range n {
		s.slots <- struct{}{}
	}
	return s
}

// Stats 返回调度器当前状态和进程生命周期累计值。
func (s *Scheduler) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tracked := 0
	for _, groups := range s.groups {
		tracked += len(groups)
	}
	c := s.counters
	return Stats{
		Capacity:             s.capacity,
		Queued:               c.queued,
		Active:               c.active,
		PeakQueued:           c.peakQueued,
		PeakActive:           c.peakActive,
		Requests:             c.requests,
		Started:              c.started,
		Completed:            c.completed,
		Errors:               c.errors,
		Canceled:             c.canceled,
		TimedOut:             c.timedOut,
		WaitDuration:         c.waitDuration,
		RunDuration:          c.runDuration,
		TrackedProcessGroups: tracked,
	}
}

func probeSandbox() sandboxKind {
	if probeBwrap() {
		return sandboxBwrap
	}
	return sandboxNone
}

// View 返回绑到 envID 和文件存储的执行视图。
func (s *Scheduler) View(envID string, files *vfs.Store) env.ExecView {
	return execView{sched: s, envID: envID, files: files}
}

type execView struct {
	sched *Scheduler
	envID string
	files *vfs.Store
}

func (v execView) Run(ctx context.Context, spec env.Cmd) (result env.ExecResult, runErr error) {
	if ctx == nil {
		panic("nil context")
	}
	if v.files == nil {
		return env.ExecResult{}, fmt.Errorf("exec: nil files")
	}
	requested := time.Now()
	v.sched.requestReceived()
	if err := ctx.Err(); err != nil {
		v.sched.requestRejected(0, false, err)
		return env.ExecResult{}, err
	}
	queued := false
	select {
	case <-v.sched.slots:
	default:
		queued = true
		v.sched.requestQueued()
		select {
		case <-v.sched.slots:
		case <-ctx.Done():
			v.sched.requestRejected(time.Since(requested), true, ctx.Err())
			return env.ExecResult{}, ctx.Err()
		}
	}
	defer func() { v.sched.slots <- struct{}{} }()
	v.sched.requestStarted(time.Since(requested), queued)
	started := time.Now()
	defer func() {
		v.sched.requestCompleted(time.Since(started), runErr)
	}()

	live, err := v.files.Materialize(v.envID)
	if err != nil {
		return env.ExecResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(live, "tmp"), 0o750); err != nil {
		return env.ExecResult{}, err
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = v.sched.timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if v.sched.run != nil {
		return v.sched.run(ctx, live, spec)
	}
	return v.sched.runSandboxed(ctx, live, spec.Command, v.envID)
}

func (s *Scheduler) requestReceived() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.requests++
}

func (s *Scheduler) requestQueued() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.queued++
	s.counters.peakQueued = max(s.counters.peakQueued, s.counters.queued)
}

func (s *Scheduler) requestStarted(wait time.Duration, queued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queued {
		s.counters.queued--
		s.counters.waitDuration += wait
	}
	s.counters.active++
	s.counters.started++
	s.counters.peakActive = max(s.counters.peakActive, s.counters.active)
}

func (s *Scheduler) requestRejected(wait time.Duration, queued bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queued {
		s.counters.queued--
		s.counters.waitDuration += wait
	}
	s.counters.completed++
	s.counters.recordError(err)
}

func (s *Scheduler) requestCompleted(took time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.active--
	s.counters.completed++
	s.counters.runDuration += took
	s.counters.recordError(err)
}

func (c *schedulerCounters) recordError(err error) {
	if err == nil {
		return
	}
	c.errors++
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.timedOut++
	case errors.Is(err, context.Canceled):
		c.canceled++
	}
}

func (s *Scheduler) runSandboxed(ctx context.Context, live, command, envID string) (env.ExecResult, error) {
	if s.sandbox != sandboxBwrap {
		return env.ExecResult{}, ErrSandboxUnavailable
	}
	return runBwrap(ctx, live, command, s.outputCap, func(pgid int) {
		s.track(envID, pgid)
	})
}

func (s *Scheduler) track(envID string, pgid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups == nil {
		s.groups = map[string][]int{}
	}
	s.groups[envID] = append(s.groups[envID], pgid)
}

// Reap 杀掉该 env 里仍活着的命令进程组。在 task 结束时调用。
func (s *Scheduler) Reap(envID string) {
	s.mu.Lock()
	pgids := s.groups[envID]
	delete(s.groups, envID)
	s.mu.Unlock()
	for _, pgid := range pgids {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

type capBuffer struct {
	buf bytes.Buffer
	cap int
	hit bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.cap > 0 {
		remain := c.cap - c.buf.Len()
		if remain <= 0 {
			c.hit = true
			return len(p), nil
		}
		if len(p) > remain {
			_, _ = c.buf.Write(p[:remain])
			c.hit = true
			return len(p), nil
		}
	}
	return c.buf.Write(p)
}

func (c *capBuffer) String() string {
	out := c.buf.String()
	if c.hit {
		out += "\n[output truncated]"
	}
	return out
}
