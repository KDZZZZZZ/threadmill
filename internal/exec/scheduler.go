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
	timeout   time.Duration
	outputCap int
	sandbox   sandboxKind
	run       func(context.Context, string, env.Cmd) (env.ExecResult, error)

	mu     sync.Mutex
	groups map[string][]int
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
		timeout:   cfg.Timeout,
		outputCap: capBytes,
		sandbox:   probeSandbox(),
	}
	for range n {
		s.slots <- struct{}{}
	}
	return s
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

func (v execView) Run(ctx context.Context, spec env.Cmd) (env.ExecResult, error) {
	if ctx == nil {
		panic("nil context")
	}
	if v.files == nil {
		return env.ExecResult{}, fmt.Errorf("exec: nil files")
	}
	select {
	case <-v.sched.slots:
	case <-ctx.Done():
		return env.ExecResult{}, ctx.Err()
	}
	defer func() { v.sched.slots <- struct{}{} }()

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
