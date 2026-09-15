package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// 后台命令照搬 Claude Code 的 run_in_background：Agent 显式声明，调用立即返回 id，
// 之后用 bash_output 读增量输出、bash_kill 终止。命令归属当前环境，不继承发起它的
// 工具调用的取消；显式终止或 Reap（角色、task 结束）都会杀掉它。bwrap 的 PID
// 命名空间与 --die-with-parent 保证它的后代、以及 Threadmill 自身崩溃时都不遗留进程。
const (
	// maxBackgroundRunning 限制每个环境同时存活的后台命令数。
	maxBackgroundRunning = 4
	// maxBackgroundRetained 限制每个环境保留的后台记录数；超出时先淘汰最早退出的。
	maxBackgroundRetained = 16
	// backgroundStopWait 是 SIGKILL 之后等待进程被回收、输出排空的上限。
	backgroundStopWait  = 2 * time.Second
	backgroundDrainWait = 100 * time.Millisecond
)

var (
	ErrBackgroundLimit       = errors.New("exec: background command limit reached")
	ErrUnknownBackground     = errors.New("exec: unknown background command")
	ErrBackgroundUnsupported = errors.New("exec: background commands require the bwrap sandbox")
)

type backgroundProc struct {
	id      string
	command string
	pgid    int
	seq     uint64
	done    chan struct{}

	mu       sync.Mutex
	out      tailBuffer
	exitCode int
	killed   bool
}

func (p *backgroundProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}

func (p *backgroundProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// status 返回当前状态，并把读位置推进到已写输出的末尾。
func (p *backgroundProc) status() env.BackgroundStatus {
	running := !p.exited()
	p.mu.Lock()
	defer p.mu.Unlock()
	output, dropped := p.out.unread()
	st := env.BackgroundStatus{
		ID:      p.id,
		Command: p.command,
		Running: running,
		Output:  output,
		Dropped: dropped,
	}
	if !running {
		st.ExitCode = p.exitCode
		st.Killed = p.killed
	}
	return st
}

// stop 杀掉整个进程组并等待它被回收。
func (p *backgroundProc) stop() error {
	if p.exited() {
		return nil
	}
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	if err := syscall.Kill(-p.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("exec: kill background command %s: %w", p.id, err)
	}
	timer := time.NewTimer(backgroundStopWait)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("exec: background command %s did not exit after SIGKILL", p.id)
	}
}

// Start 在当前环境的 live 目录里启动后台命令并立即返回。
func (v execView) Start(ctx context.Context, spec env.Cmd) (env.BackgroundStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return env.BackgroundStatus{}, err
	}
	if v.files == nil {
		return env.BackgroundStatus{}, fmt.Errorf("exec: nil files")
	}
	if spec.Command == "" {
		return env.BackgroundStatus{}, fmt.Errorf("exec: empty background command")
	}
	if v.sched.sandbox != sandboxBwrap {
		return env.BackgroundStatus{}, ErrBackgroundUnsupported
	}
	live, err := v.files.Materialize(v.envID)
	if err != nil {
		return env.BackgroundStatus{}, err
	}
	runtimeDir, err := v.sched.runtimeDir(ctx, v.envID, live)
	if err != nil {
		return env.BackgroundStatus{}, err
	}
	return v.sched.startBackground(v.envID, live, runtimeDir, spec.Command)
}

// Output 返回后台命令的状态与新输出。wait > 0 时最多等待这么久，命令退出即提前返回。
func (v execView) Output(ctx context.Context, id string, wait time.Duration) (env.BackgroundStatus, error) {
	if ctx == nil {
		panic("nil context")
	}
	p := v.sched.lookupBackground(v.envID, id)
	if p == nil {
		return env.BackgroundStatus{}, fmt.Errorf("%w: %s", ErrUnknownBackground, id)
	}
	if wait > 0 && !p.exited() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
		case <-ctx.Done():
			return env.BackgroundStatus{}, ctx.Err()
		}
	}
	return p.status(), nil
}

// Kill 终止后台命令及其全部后代，返回最终状态并释放记录。
func (v execView) Kill(id string) (env.BackgroundStatus, error) {
	p := v.sched.lookupBackground(v.envID, id)
	if p == nil {
		return env.BackgroundStatus{}, fmt.Errorf("%w: %s", ErrUnknownBackground, id)
	}
	if err := p.stop(); err != nil {
		return p.status(), err
	}
	v.sched.forgetBackground(v.envID, id)
	return p.status(), nil
}

func (s *Scheduler) startBackground(envID, live, runtimeDir, command string) (env.BackgroundStatus, error) {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	procs := s.background[envID]
	running := 0
	for _, p := range procs {
		if !p.exited() {
			running++
		}
	}
	if running >= maxBackgroundRunning {
		return env.BackgroundStatus{}, fmt.Errorf(
			"%w: %d already running in this workspace; bash_kill one first",
			ErrBackgroundLimit, running,
		)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return env.BackgroundStatus{}, err
	}
	cmd := osexec.Command("bwrap", bwrapCommandArgs(live, runtimeDir, bashArgs(command))...)
	cmd.Env = networkSandboxEnv("/tmp", "/tmp")
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return env.BackgroundStatus{}, fmt.Errorf("exec: start background command: %w", err)
	}
	_ = pw.Close()

	s.bgSeq++
	p := &backgroundProc{
		id:      "bg-" + strconv.FormatUint(s.bgSeq, 10),
		command: command,
		pgid:    cmd.Process.Pid,
		seq:     s.bgSeq,
		done:    make(chan struct{}),
	}
	p.out.cap = s.outputCap
	s.track(envID, p.pgid)
	if s.background == nil {
		s.background = make(map[string]map[string]*backgroundProc)
	}
	if procs == nil {
		procs = make(map[string]*backgroundProc)
		s.background[envID] = procs
	}
	procs[p.id] = p
	pruneExitedBackground(procs)
	go s.superviseBackground(envID, cmd, pr, p)
	return p.status(), nil
}

// superviseBackground 排空输出并在进程退出后记录结果。它随进程退出而结束：
// Kill 与 Reap 发 SIGKILL，PID 命名空间让所有后代同时退出、管道随之关闭。
func (s *Scheduler) superviseBackground(envID string, cmd *osexec.Cmd, pr *os.File, p *backgroundProc) {
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(p, pr)
	}()
	err := cmd.Wait()
	drain := time.NewTimer(backgroundDrainWait)
	select {
	case <-copied:
		drain.Stop()
	case <-drain.C:
		_ = pr.Close()
		<-copied
	}
	_ = pr.Close()

	code := 0
	var exitErr *osexec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		code = -1
		_, _ = p.Write([]byte("\n[wait: " + err.Error() + "]"))
	}
	p.mu.Lock()
	p.exitCode = code
	p.mu.Unlock()
	s.untrack(envID, p.pgid)
	close(p.done)
}

func (s *Scheduler) lookupBackground(envID, id string) *backgroundProc {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	return s.background[envID][id]
}

func (s *Scheduler) forgetBackground(envID, id string) {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	procs := s.background[envID]
	delete(procs, id)
	if len(procs) == 0 {
		delete(s.background, envID)
	}
}

// stopBackground 终止该环境的全部后台命令；全部确认退出后才丢弃记录。
func (s *Scheduler) stopBackground(envID string) error {
	s.bgMu.Lock()
	procs := make([]*backgroundProc, 0, len(s.background[envID]))
	for _, p := range s.background[envID] {
		procs = append(procs, p)
	}
	s.bgMu.Unlock()
	var err error
	for _, p := range procs {
		err = errors.Join(err, p.stop())
	}
	if err != nil {
		return err
	}
	s.bgMu.Lock()
	delete(s.background, envID)
	s.bgMu.Unlock()
	return nil
}

func (s *Scheduler) backgroundStats() (running int, started uint64) {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	for _, procs := range s.background {
		for _, p := range procs {
			if !p.exited() {
				running++
			}
		}
	}
	return running, s.bgSeq
}

// pruneExitedBackground 在记录超出上限时淘汰最早退出的命令；运行中的从不淘汰。
func pruneExitedBackground(procs map[string]*backgroundProc) {
	for len(procs) > maxBackgroundRetained {
		var oldest *backgroundProc
		for _, p := range procs {
			if p.exited() && (oldest == nil || p.seq < oldest.seq) {
				oldest = p
			}
		}
		if oldest == nil {
			return
		}
		delete(procs, oldest.id)
	}
}

// tailBuffer 只保留最近 cap 字节，并记录读者错过的字节数。
type tailBuffer struct {
	data    []byte
	cap     int
	written int64
	read    int64
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.written += int64(n)
	if b.cap > 0 && n >= b.cap {
		b.data = append(b.data[:0], p[n-b.cap:]...)
		return n, nil
	}
	b.data = append(b.data, p...)
	if excess := len(b.data) - b.cap; b.cap > 0 && excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:b.cap]
	}
	return n, nil
}

// unread 返回上次读取之后仍保留的输出，以及其间被丢弃的字节数。
func (b *tailBuffer) unread() (string, int64) {
	start := b.written - int64(len(b.data))
	var dropped int64
	if b.read < start {
		dropped = start - b.read
		b.read = start
	}
	out := string(b.data[b.read-start:])
	b.read = b.written
	return out, dropped
}
