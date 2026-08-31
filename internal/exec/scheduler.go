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

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

var ErrSandboxUnavailable = errors.New("exec: SANDBOX_UNAVAILABLE")

const defaultOutputCapKB = 256

type sandboxKind int

const (
	sandboxNone sandboxKind = iota
	sandboxBwrap
	sandboxDocker
	sandboxExternal
)

// Config 是执行调度器的槽位、超时和输出上限。
type Config struct {
	Slots          int
	Timeout        time.Duration
	OutputCapKB    int
	ContainerImage string
	// ExternalSandbox trusts the caller's process boundary for isolation.
	ExternalSandbox bool
	// HeavySlots 限制重命令（历史均值超过 HeavyThreshold）的并发；0 时取 slots/8（至少 1）。
	HeavySlots int
	// HeavyThreshold 判定重命令的历史平均时长阈值；0 时取 10s。
	HeavyThreshold time.Duration
	// MemoryBudgetBytes 按历史峰值 RSS 做记账准入；0 关闭。
	MemoryBudgetBytes int64
	// Cache 打开命令结果缓存：依赖文件版本一致的环境可以复用彼此的执行
	// 结果与产物。nil 表示关闭。
	Cache *cmdcache.Cache
	// DisableTrace 关闭系统调用追踪。追踪不可用时缓存仍能工作，
	// 只是退化成整树指纹键：命中率低，但绝不会错命中。
	DisableTrace bool
}

// Scheduler 用信号量限制并发，并把命令跑进某个 env 的 live 目录。
type Scheduler struct {
	slots     chan struct{}
	capacity  int
	timeout   time.Duration
	outputCap int
	sandbox   sandboxKind
	image     string
	run       func(context.Context, string, env.Cmd) (env.ExecResult, error)

	mu       sync.Mutex
	groups   map[string][]int
	runtimes map[string]string
	counters schedulerCounters
	costs    *costTable

	cache   *cmdcache.Cache
	tracing bool

	heavy          chan struct{}
	heavyThreshold time.Duration
	memMu          sync.Mutex
	memInUse       int64
	memBudget      int64
}

type schedulerCounters struct {
	requests          uint64
	started           uint64
	completed         uint64
	errors            uint64
	canceled          uint64
	timedOut          uint64
	queued            int
	active            int
	peakQueued        int
	peakActive        int
	waitDuration      time.Duration
	runDuration       time.Duration
	heavyQueued       int
	heavyActive       int
	heavyPeakQueued   int
	heavyPeakActive   int
	heavyWaitDuration time.Duration
}

// Stats 是执行槽位、排队和完成情况的并发一致快照。
type Stats struct {
	Capacity             int           `json:"capacity"`
	SandboxBackend       string        `json:"sandbox_backend"`
	NetworkIsolation     string        `json:"network_isolation"`
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
	RuntimeDirs          int           `json:"runtime_dirs"`
	HeavyCapacity        int           `json:"heavy_capacity"`
	HeavyQueued          int           `json:"heavy_queued"`
	HeavyActive          int           `json:"heavy_active"`
	HeavyPeakQueued      int           `json:"heavy_peak_queued"`
	HeavyPeakActive      int           `json:"heavy_peak_active"`
	HeavyWaitDuration    time.Duration `json:"heavy_wait_duration"`
	// Cache 是命令结果缓存的累计计数；未启用时为零值。
	Cache cmdcache.Stats `json:"cache"`
	// DependencyTracing 表示读集推断是否可用。为假时缓存退化成整树指纹键。
	DependencyTracing bool `json:"dependency_tracing"`
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
		image:     cfg.ContainerImage,
		costs:     newCostTable(),
	}
	heavySlots := cfg.HeavySlots
	if heavySlots <= 0 {
		heavySlots = max(1, n/8)
	}
	s.heavy = make(chan struct{}, heavySlots)
	for range heavySlots {
		s.heavy <- struct{}{}
	}
	s.heavyThreshold = cfg.HeavyThreshold
	if s.heavyThreshold <= 0 {
		s.heavyThreshold = defaultHeavyThreshold
	}
	s.memBudget = cfg.MemoryBudgetBytes
	s.cache = cfg.Cache
	s.tracing = cfg.Cache != nil && !cfg.DisableTrace && tracerPath() != ""
	if cfg.ExternalSandbox {
		s.sandbox = sandboxExternal
	} else {
		s.sandbox = probeSandbox(cfg.ContainerImage)
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
	backend, network := s.isolationBoundary()
	return Stats{
		Capacity:             s.capacity,
		SandboxBackend:       backend,
		NetworkIsolation:     network,
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
		RuntimeDirs:          len(s.runtimes),
		HeavyCapacity:        cap(s.heavy),
		HeavyQueued:          c.heavyQueued,
		HeavyActive:          c.heavyActive,
		HeavyPeakQueued:      c.heavyPeakQueued,
		HeavyPeakActive:      c.heavyPeakActive,
		HeavyWaitDuration:    c.heavyWaitDuration,
		Cache:                s.cache.Stats(),
		DependencyTracing:    s.tracing,
	}
}

func (s *Scheduler) isolationBoundary() (backend, network string) {
	switch s.sandbox {
	case sandboxBwrap:
		return "bwrap", "shared"
	case sandboxDocker:
		return "docker", "disabled"
	case sandboxExternal:
		return "external", "external"
	default:
		return "unavailable", "unavailable"
	}
}

func probeSandbox(containerImage string) sandboxKind {
	if probeBwrap() {
		return sandboxBwrap
	}
	if containerImage != "" && probeDocker(containerImage) {
		return sandboxDocker
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
	// 物化发生在拿槽之前：环境准备不占用命令槽位，拷贝时间不计入排队。
	live, err := v.files.Materialize(v.envID)
	if err != nil {
		return env.ExecResult{}, err
	}

	var segments []commandSegment
	if v.sched.cacheEnabled() {
		segments = splitCacheCommand(spec.Command)
	}
	segmented := len(segments) > 0
	if !segmented {
		segments = []commandSegment{{command: spec.Command}}
	}
	var combined capBuffer
	combined.cap = v.sched.outputCap
	lastExit := 0
	admitted := false
	var started time.Time
	var release func(error)
	defer func() {
		if !admitted {
			return
		}
		v.sched.costs.record(spec.Command, time.Since(started), result.PeakRSSBytes)
		release(runErr)
	}()

segmentLoop:
	for _, segment := range segments {
		if !segment.runnable(lastExit) {
			continue
		}
		if err := ctx.Err(); err != nil {
			if !admitted {
				v.sched.requestRejected(time.Since(requested), false, err)
			}
			runErr = err
			return result, runErr
		}
		command := segment.cacheCommand(lastExit)
		// 缓存查找在拿槽之前；分段命中会先回放产物，后段立即看得到。
		key := v.sched.cacheKey(command)
		hit := v.sched.lookupCache(live, key)
		verifying := false
		if hit != nil {
			switch {
			case v.sched.cache.ShouldVerify():
				v.sched.cache.RecordVerification()
				verifying = true
			case v.sched.cache.Replay(live, hit) == nil:
				out := cachedResult(hit)
				lastExit = out.ExitCode
				result.ExitCode = lastExit
				if segmented {
					if _, err := combined.Write([]byte(out.Output)); err != nil {
						runErr = fmt.Errorf("exec: combine cached output: %w", err)
						return result, runErr
					}
				} else {
					result = out
				}
				if lastExit < 0 {
					break segmentLoop
				}
				continue
			default:
				hit = nil
			}
		}
		if !admitted {
			ctx, started, release, err = v.admit(ctx, requested, spec)
			if err != nil {
				return result, err
			}
			admitted = true
		}

		segmentSpec := spec
		segmentSpec.Command = command
		segmentStarted := time.Now()
		var out env.ExecResult
		var trace *traceRun
		if v.sched.run != nil {
			out, runErr = v.sched.run(ctx, live, segmentSpec)
		} else {
			out, trace, runErr = v.sched.runSandboxed(ctx, live, command, v.envID, v.sched.tracing)
		}
		took := time.Since(segmentStarted)
		fresh := v.sched.storeTrace(ctx, live, key, trace, out, runErr, took)
		if verifying {
			v.sched.reconcileVerification(key, hit, fresh)
		}
		lastExit = out.ExitCode
		result.ExitCode = lastExit
		result.PeakRSSBytes = max(result.PeakRSSBytes, out.PeakRSSBytes)
		if segmented {
			if _, err := combined.Write([]byte(out.Output)); err != nil {
				runErr = fmt.Errorf("exec: combine output: %w", err)
				return result, runErr
			}
			result.Output = combined.String()
		} else {
			result = out
		}
		if runErr != nil {
			return result, runErr
		}
		if lastExit < 0 {
			break
		}
	}
	if !admitted {
		v.sched.requestServedFromCache()
	}
	if segmented {
		result.Output = combined.String()
	}
	return result, nil
}

func (v execView) admit(
	ctx context.Context,
	requested time.Time,
	spec env.Cmd,
) (context.Context, time.Time, func(error), error) {
	heavy, estRSS := v.sched.commandClass(spec.Command)
	if !v.sched.reserveMemory(ctx, estRSS) {
		err := ctx.Err()
		v.sched.requestRejected(time.Since(requested), false, err)
		return ctx, time.Time{}, nil, err
	}
	heavyAcquired := false
	if heavy {
		heavyQueued := false
		heavyRequested := time.Now()
		select {
		case <-v.sched.heavy:
		default:
			heavyQueued = true
			v.sched.heavyRequestQueued()
			select {
			case <-v.sched.heavy:
			case <-ctx.Done():
				v.sched.heavyRequestRejected(time.Since(heavyRequested))
				v.sched.releaseMemory(estRSS)
				v.sched.requestRejected(time.Since(requested), false, ctx.Err())
				return ctx, time.Time{}, nil, ctx.Err()
			}
		}
		heavyAcquired = true
		v.sched.heavyRequestStarted(time.Since(heavyRequested), heavyQueued)
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
			if heavyAcquired {
				v.sched.heavy <- struct{}{}
				v.sched.heavyRequestCompleted()
			}
			v.sched.releaseMemory(estRSS)
			v.sched.requestRejected(time.Since(requested), true, ctx.Err())
			return ctx, time.Time{}, nil, ctx.Err()
		}
	}
	v.sched.requestStarted(time.Since(requested), queued)
	started := time.Now()
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = v.sched.timeout
	}
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	release := func(runErr error) {
		if cancel != nil {
			cancel()
		}
		v.sched.requestCompleted(time.Since(started), runErr)
		v.sched.slots <- struct{}{}
		if heavyAcquired {
			v.sched.heavy <- struct{}{}
			v.sched.heavyRequestCompleted()
		}
		v.sched.releaseMemory(estRSS)
	}
	return ctx, started, release, nil
}

// requestServedFromCache 记一次由缓存直接满足的请求。
// 有意不动 started：这条命令没有真的跑起来，也没有占用执行槽位。
func (s *Scheduler) requestServedFromCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.completed++
}

// CostStats 返回按命令指纹累计的时长与 RSS 峰值画像，供调度分层与容量规划使用。
func (s *Scheduler) CostStats() CostSnapshot {
	if s == nil || s.costs == nil {
		return CostSnapshot{}
	}
	return s.costs.snapshot()
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

func (s *Scheduler) heavyRequestQueued() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.heavyQueued++
	s.counters.heavyPeakQueued = max(s.counters.heavyPeakQueued, s.counters.heavyQueued)
}

func (s *Scheduler) heavyRequestStarted(wait time.Duration, queued bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if queued {
		s.counters.heavyQueued--
		s.counters.heavyWaitDuration += wait
	}
	s.counters.heavyActive++
	s.counters.heavyPeakActive = max(s.counters.heavyPeakActive, s.counters.heavyActive)
}

func (s *Scheduler) heavyRequestRejected(wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.heavyQueued--
	s.counters.heavyWaitDuration += wait
}

func (s *Scheduler) heavyRequestCompleted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counters.heavyActive--
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

// runSandboxed 在隔离环境里执行命令，并在请求时一并产出系统调用追踪。
//
// 返回的 traceRun 携带追踪文件路径与沙箱路径映射；调用方负责解析并删除它。
// docker 后端不支持追踪：它以 --cap-drop=ALL 和 no-new-privileges 启动，
// ptrace 被挡住，而放宽隔离换缓存不是可接受的交换。
func (s *Scheduler) runSandboxed(
	ctx context.Context,
	live, command, envID string,
	tracing bool,
) (env.ExecResult, *traceRun, error) {
	defer s.pruneDeadProcessGroups(envID)
	tracer := ""
	if tracing {
		tracer = tracerPath()
	}
	switch s.sandbox {
	case sandboxBwrap:
		runtimeDir, err := s.runtimeDir(envID, live)
		if err != nil {
			return env.ExecResult{}, nil, err
		}
		// live 位于独立的 /workspace；/proc 等系统挂载不再落进项目根，
		// 追踪器仍可用同一个前缀把访问映射回工作区相对路径。
		trace, err := newTraceRun(tracer, live, runtimeDir, bwrapWorkspace, "/tmp")
		if err != nil {
			return env.ExecResult{}, nil, err
		}
		track := trace.tracker(func(pgid int) {
			s.track(envID, pgid)
		})
		result, err := runBwrap(ctx, live, runtimeDir, command, s.outputCap, track, trace)
		if err == nil && trace != nil && trace.finish() {
			s.untrack(envID, trace.pgid)
		}
		return result, trace, err
	case sandboxDocker:
		result, err := runDocker(ctx, live, command, s.image, s.outputCap, func(pgid int) {
			s.track(envID, pgid)
		})
		return result, nil, err
	case sandboxExternal:
		runtimeDir, err := s.runtimeDir(envID, live)
		if err != nil {
			return env.ExecResult{}, nil, err
		}
		trace, err := newTraceRun(tracer, live, runtimeDir, live, runtimeDir)
		if err != nil {
			return env.ExecResult{}, nil, err
		}
		track := trace.tracker(func(pgid int) {
			s.track(envID, pgid)
		})
		result, err := runExternalSandbox(ctx, live, runtimeDir, command, s.outputCap, track, trace)
		if err == nil && trace != nil && trace.finish() {
			s.untrack(envID, trace.pgid)
		}
		return result, trace, err
	default:
		return env.ExecResult{}, nil, ErrSandboxUnavailable
	}
}

func (s *Scheduler) runtimeDir(envID, live string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir := s.runtimes[envID]; dir != "" {
		return dir, nil
	}
	dir, err := os.MkdirTemp(filepath.Dir(live), ".threadmill-exec-")
	if err != nil {
		return "", fmt.Errorf("exec: create runtime dir: %w", err)
	}
	if s.runtimes == nil {
		s.runtimes = make(map[string]string)
	}
	s.runtimes[envID] = dir
	return dir, nil
}

func (s *Scheduler) track(envID string, pgid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups == nil {
		s.groups = map[string][]int{}
	}
	s.groups[envID] = append(s.groups[envID], pgid)
}

func (s *Scheduler) untrack(envID string, pgid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups := s.groups[envID]
	kept := groups[:0]
	for _, group := range groups {
		if group != pgid {
			kept = append(kept, group)
		}
	}
	if len(kept) == 0 {
		delete(s.groups, envID)
		return
	}
	s.groups[envID] = kept
}

func (s *Scheduler) pruneDeadProcessGroups(envID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups := s.groups[envID]
	live := groups[:0]
	for _, pgid := range groups {
		if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
			live = append(live, pgid)
		}
	}
	if len(live) == 0 {
		delete(s.groups, envID)
		return
	}
	s.groups[envID] = live
}

const (
	runtimeCleanupTimeout = time.Second
	runtimeCleanupQuiet   = 10 * time.Millisecond
	runtimeCleanupPoll    = time.Millisecond
)

// Reap 杀掉该 env 里仍活着的命令进程组并删除运行时目录。在 task 结束时调用。
func (s *Scheduler) Reap(envID string) error {
	s.mu.Lock()
	pgids := append([]int(nil), s.groups[envID]...)
	runtimeDir := s.runtimes[envID]
	s.mu.Unlock()
	var err error
	for _, pgid := range pgids {
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			err = errors.Join(err, fmt.Errorf("exec: reap process group %d: %w", pgid, killErr))
		}
	}
	if err != nil {
		return err
	}
	if runtimeDir != "" {
		remove := os.RemoveAll
		if s.sandbox == sandboxExternal {
			remove = removeExternalRuntimeDir
		}
		if removeErr := remove(runtimeDir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("exec: remove runtime dir: %w", removeErr))
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.groups, envID)
	delete(s.runtimes, envID)
	s.mu.Unlock()
	return nil
}

// removeExternalRuntimeDir tolerates short-lived sidecars that call setsid and
// outlive the foreground process group. Go telemetry is one such process. The
// external sandbox owns process isolation, so Threadmill waits for a quiet
// runtime directory instead of pretending the original process group owns all
// descendants.
func removeExternalRuntimeDir(dir string) error {
	deadline := time.Now().Add(runtimeCleanupTimeout)
	var lastErr error
	for {
		if err := os.RemoveAll(dir); err != nil {
			if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return err
			}
			lastErr = err
		} else {
			quiet, err := runtimeDirRemainedAbsent(dir, runtimeCleanupQuiet)
			if err != nil {
				return err
			}
			if quiet {
				return nil
			}
			lastErr = fmt.Errorf("runtime directory %q was recreated during cleanup", dir)
		}
		if !time.Now().Before(deadline) {
			return lastErr
		}
		time.Sleep(runtimeCleanupPoll)
	}
}

func runtimeDirRemainedAbsent(dir string, quiet time.Duration) (bool, error) {
	deadline := time.Now().Add(quiet)
	for {
		_, err := os.Lstat(dir)
		switch {
		case err == nil:
			return false, nil
		case !errors.Is(err, os.ErrNotExist):
			return false, err
		case !time.Now().Before(deadline):
			return true, nil
		default:
			time.Sleep(runtimeCleanupPoll)
		}
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
