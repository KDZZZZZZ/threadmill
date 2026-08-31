// Package manager 把经理循环、协调图调度和任务报告串成一个可唤醒的运行单元。
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const metricsSnapshotInterval = 30 * time.Second

// Options 启动 manager 所需的工作区、配置和可选依赖。
type Options struct {
	Root       string
	ConfigPath string
	File       provider.FileConfig
	Provider   agent.Provider
	Output     func(string)
	OnEvent    event.Handler
	Logger     *slog.Logger
}

// Manager 是长命经理加串行任务调度。
type Manager struct {
	graph       *coordination.Graph
	stores      coordination.Stores
	assemble    coordination.AssembleFunc
	loop        *agent.Loop
	tokens      *tokenCounter
	metrics     *event.Collector
	events      *event.Bus
	output      func(string)
	modelName   string
	startedAt   time.Time
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	mu          sync.Mutex
	inputs      []managerInput
	pending     int
	idle        *sync.Cond
	err         error
	settling    bool
	cancelRun   context.CancelFunc
	taskRunning bool
	logFile     io.Closer
	logger      *slog.Logger
}

// managerInput 只保存与 loop FIFO 同步的投影元数据；消息本体由 loop 持有。
type managerInput struct {
	projectUserMessage bool
}

// Open 接线存储、装配经理并启动常驻 Run。
func Open(parent context.Context, opt Options) (*Manager, error) {
	if parent == nil {
		panic("nil context")
	}
	if opt.Root == "" {
		return nil, fmt.Errorf("manager: root is required")
	}
	paths, err := openStatePaths(opt.Root)
	if err != nil {
		return nil, err
	}
	opt.Root = paths.ProjectRoot
	file := opt.File
	if file.LLM.Provider == "" {
		loaded, err := provider.LoadRuntimeConfig(opt.Root, opt.ConfigPath)
		if err != nil {
			return nil, err
		}
		file = loaded
	}
	llm := opt.Provider
	if llm == nil {
		got, err := provider.NewResponses(file.LLM, nil)
		if err != nil {
			return nil, err
		}
		llm = got
	}

	checkpoints, err := agent.NewDirCheckpointStore(paths.ReactDir)
	if err != nil {
		return nil, err
	}
	managerCheckpoints, err := agent.NewDirCheckpointStore(paths.ManagerReactDir)
	if err != nil {
		return nil, err
	}
	progress, err := coordination.NewDirProgressStore(paths.ProgressDir)
	if err != nil {
		return nil, err
	}
	graph, err := coordination.OpenGraph(paths.GraphFile)
	if err != nil {
		return nil, err
	}
	memory, err := ctxgraph.OpenStore(paths.MemoryFile)
	if err != nil {
		return nil, err
	}
	liveRoot := paths.VFSDir
	if file.VFS.LiveRoot != "" {
		liveRoot = file.VFS.LiveRoot
	}
	files, err := vfs.NewPersistentStoreWithOptions(
		opt.Root,
		liveRoot,
		vfs.Options{Overlay: true},
	)
	if err != nil {
		return nil, err
	}
	if file.Memory.SoftMemoryLimitMB > 0 {
		debug.SetMemoryLimit(int64(file.Memory.SoftMemoryLimitMB) << 20)
	}
	// 命令结果缓存跨进程共享：缓存目录挂在项目状态目录下，同一项目的另一个
	// Threadmill 进程指向同一份产物存储。构造失败不该让整个进程起不来，
	// 缓存只是加速，不是正确性的一部分。
	var commandCache *cmdcache.Cache
	if file.Exec.Cache.Enabled {
		commandCache, err = cmdcache.New(cmdcache.Config{
			Dir:              paths.CacheDir,
			MaxBytes:         file.Exec.Cache.MaxBytes,
			MaxReadSet:       file.Exec.Cache.MaxReadSet,
			CacheFailures:    file.Exec.Cache.CacheFailures,
			VerifySampleRate: file.Exec.Cache.VerifySampleRate,
		})
		if err != nil {
			return nil, err
		}
	}

	s := &Manager{
		graph:     graph,
		tokens:    newTokenCounter(),
		metrics:   event.NewCollector(),
		output:    opt.Output,
		modelName: file.LLM.Model,
		startedAt: time.Now(),
	}
	s.idle = sync.NewCond(&s.mu)
	s.stores = coordination.Stores{
		Memory: memory,
		Files:  files,
		Exec: tmexec.New(tmexec.Config{
			Slots:           file.Exec.Slots,
			Timeout:         time.Duration(file.Exec.Timeout) * time.Second,
			OutputCapKB:     file.Exec.OutputCapKB,
			ContainerImage:  file.Exec.ContainerImage,
			ExternalSandbox: file.Exec.ExternalSandbox,
			Cache:           commandCache,
			DisableTrace:    file.Exec.Cache.DisableTrace,
		}),
	}
	s.graph.SetProgressStore(progress)
	if err := s.graph.SetTaskSink(s.stores.ProjectManagerTaskInfos); err != nil {
		return nil, err
	}

	logger := opt.Logger
	if logger == nil {
		f, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		logger = logging.New(logging.Config{Output: f})
		s.logFile = f
	}
	s.logger = logger
	fileStats := files.Stats()
	if fileStats.OverlayAvailable {
		logger.Info("VFS materialization acceleration available", "backend", fileStats.OverlayBackend)
	} else if !vfs.ReflinkCloneable(opt.Root, liveRoot) {
		logger.Warn("materialize cannot use reflink clones (live root not on a reflink filesystem, or base repo and live root on different devices); each environment will full-copy the repo",
			"live_root", liveRoot)
	}
	bus := event.NewBus(s.onEvent, s.metrics.Handle, event.Monitor(logger), opt.OnEvent)
	s.events = bus
	overlay := agent.FileOverlay{
		Tools:    file.Tools,
		Prompts:  file.Prompts,
		Events:   bus,
		Curation: file.Memory.Curation,
	}
	overlay.NamedTools = s.graph.HelpTools(s.enqueueManager)
	s.assemble = coordination.Assemble(
		s.stores,
		llm,
		file.Agents,
		nil,
		file.LLM.ContextWindow,
		checkpoints,
		overlay,
	)
	loop, err := coordination.NewManagerLoop(
		s.graph,
		s.stores,
		llm,
		file.Agents,
		nil,
		file.LLM.ContextWindow,
		overlay,
	)
	if err != nil {
		return nil, err
	}
	if err := loop.AddHooks(s.hooks()); err != nil {
		return nil, err
	}
	loop.BindCheckpointStore(managerCheckpoints)
	managerPending, err := loop.HasPendingCheckpoint()
	if err != nil {
		return nil, err
	}
	if managerPending {
		s.inputs = append(s.inputs, managerInput{})
		s.pending++
	}
	s.loop = loop
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	ready := make(chan struct{})
	if err := loop.AddHooks(agent.Hooks{
		BeforeRun: []agent.RunHook{func(context.Context) error {
			close(ready)
			return nil
		}},
	}); err != nil {
		cancel()
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := loop.Run(ctx)
		s.setErr(err)
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		cancel()
		s.wg.Wait()
		return nil, ctx.Err()
	}
	if !managerPending {
		err = s.runReady(ctx)
	}
	if err != nil {
		cancel()
		s.wg.Wait()
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(metricsSnapshotInterval)
		defer ticker.Stop()
		s.monitorSnapshots(ctx, ticker.C)
	}()
	return s, nil
}

// Send 把用户消息加入 loop FIFO；消息只在对应 turn 开始时进入 manager 记忆。
func (s *Manager) Send(text string) {
	s.enqueue(text, true)
}

// WaitIdle 等到经理队列清空且没有正在跑的任务。
func (s *Manager) WaitIdle(ctx context.Context) error {
	if ctx == nil {
		panic("nil context")
	}
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.idle.Broadcast()
		s.mu.Unlock()
	})
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for (((s.pending > 0 || s.taskRunning) && s.err == nil) || s.settling) && ctx.Err() == nil {
		s.idle.Wait()
	}
	if s.err != nil && !errors.Is(s.err, context.Canceled) {
		return s.err
	}
	return ctx.Err()
}

// Snapshot 返回当前协调图。
func (s *Manager) Snapshot() coordination.Snapshot {
	return s.graph.Snapshot()
}

// Cancel 取消正在跑的任务树或抢占经理当前轮；没有可取消的工作时返回 false。
func (s *Manager) Cancel() bool {
	s.mu.Lock()
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return s.loop.Preempt()
}

// ModelName 返回配置里的 LLM 模型名。
func (s *Manager) ModelName() string {
	return s.modelName
}

// Busy 表示还有未完成的经理轮或任务。
func (s *Manager) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending > 0 || s.taskRunning || s.settling
}

// Metrics 返回 manager、事件、调度器、VFS、记忆图和 Go runtime 的一致近照。
func (s *Manager) Metrics() Metrics {
	s.mu.Lock()
	pending := s.pending
	taskRunning := s.taskRunning
	startedAt := s.startedAt
	s.mu.Unlock()

	var memoryStats runtime.MemStats
	runtime.ReadMemStats(&memoryStats)
	metrics := Metrics{
		Time:        time.Now(),
		Uptime:      time.Since(startedAt),
		Pending:     pending,
		TaskRunning: taskRunning,
		Events:      s.metrics.Snapshot(),
		Runtime: RuntimeMetrics{
			Goroutines:   runtime.NumGoroutine(),
			HeapAlloc:    memoryStats.HeapAlloc,
			HeapObjects:  memoryStats.HeapObjects,
			GCCount:      memoryStats.NumGC,
			GCPauseTotal: time.Duration(memoryStats.PauseTotalNs),
		},
	}
	if s.stores.Exec != nil {
		metrics.Exec = s.stores.Exec.Stats()
	}
	if s.stores.Files != nil {
		metrics.VFS = s.stores.Files.Stats()
	}
	if s.stores.Memory != nil {
		metrics.Memory = s.stores.Memory.Stats()
	}
	for _, task := range s.graph.Snapshot().Tasks {
		metrics.Tasks.Total++
		switch task.Outcome {
		case coordination.OutcomeActive:
			metrics.Tasks.Active++
		case coordination.OutcomeDone:
			metrics.Tasks.Done++
		case coordination.OutcomeFailed:
			metrics.Tasks.Failed++
		case coordination.OutcomeCanceled:
			metrics.Tasks.Canceled++
		}
	}
	return metrics
}

// Close 停掉经理循环。
func (s *Manager) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.stores.Files != nil {
		if err := s.stores.Files.Close(); err != nil && s.logger != nil {
			s.logger.Warn("close VFS", "error", err)
		}
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

func (s *Manager) hooks() agent.Hooks {
	return agent.Hooks{
		BeforeTurn: []agent.TurnHook{
			func(_ context.Context, message agent.UserMessage) error {
				s.mu.Lock()
				if len(s.inputs) == 0 {
					s.mu.Unlock()
					return errors.New("manager: missing queued input metadata")
				}
				input := s.inputs[0]
				s.inputs[0] = managerInput{}
				s.inputs = s.inputs[1:]
				s.mu.Unlock()
				if !input.projectUserMessage {
					return nil
				}
				return s.stores.ProjectManagerUserMessage(message.Content)
			},
		},
		AfterAssistant: []agent.AfterAssistantHook{
			func(_ context.Context, message agent.AssistantMessage) error {
				if len(message.ToolCalls) > 0 || message.Content == "" || s.output == nil {
					return nil
				}
				s.output(message.Content)
				return nil
			},
		},
		AfterTurn: []agent.AfterTurnHook{
			func(ctx context.Context, user agent.UserMessage, result agent.TurnResult) error {
				if result.Err != nil && !errors.Is(result.Err, context.Canceled) {
					s.setErr(result.Err)
					return nil
				}
				if requestID, ok := coordination.ParseHelpRequestID(user.Content); ok {
					if err := s.graph.DeclineHelp(requestID); err != nil {
						s.setErr(err)
						return err
					}
				}
				err := s.runReady(ctx)
				if err != nil {
					s.setErr(err)
					return err
				}
				s.turnDone()
				return nil
			},
		},
	}
}

func (s *Manager) runReady(ctx context.Context) error {
	s.mu.Lock()
	running := s.taskRunning
	s.mu.Unlock()
	if running {
		return nil
	}

	var root coordination.Task
	for _, task := range s.graph.Snapshot().Tasks {
		if task.SpawnedFrom != "" || task.Outcome != coordination.OutcomeActive {
			continue
		}
		// root 串行执行且后继 root 的环境从队头 fork：队头 held 时整条队列停住，
		// 不能越过它启动后面的 root，否则会 fork 到一个还没运行过的环境。
		if task.RunPolicy == coordination.RunPolicyHeld {
			return nil
		}
		root = task
		break
	}
	if root.ID == "" {
		return nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.taskRunning {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.taskRunning = true
	s.cancelRun = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.runRoot(runCtx, cancel, root)
	}()
	return nil
}

func (s *Manager) runRoot(ctx context.Context, cancel context.CancelFunc, task coordination.Task) {
	defer cancel()
	started := time.Now()
	s.events.Publish(ctx, event.TaskStart(task.ID))
	before := s.tokens.sumPrefix(task.ID + ":")
	var report string
	reported := false
	_, runErr := s.graph.RunWithReport(
		ctx, task.ID, task.Info, s.stores, s.assemble,
		func(latest coordination.Task, output string, taskErr error) error {
			tokens := s.tokens.sumPrefix(task.ID+":") - before
			report = formatReport(latest, output, taskErr, time.Since(started), tokens)
			if err := s.stores.ProjectManagerTaskReport(latest, report); err != nil {
				return err
			}
			reported = true
			return nil
		},
	)
	latest, _ := s.graph.Task(task.ID)
	s.events.Publish(ctx, event.TaskEnd(task.ID, latest.Outcome, started, runErr))
	if latest.Outcome == coordination.OutcomeActive || !reported {
		s.mu.Lock()
		s.taskRunning = false
		s.cancelRun = nil
		s.mu.Unlock()
		s.setErr(runErr)
		return
	}

	if s.output != nil {
		s.output(report)
	}
	s.mu.Lock()
	s.taskRunning = false
	s.cancelRun = nil
	s.enqueueLocked(report, false)
	s.mu.Unlock()
}

func (s *Manager) enqueueManager(text string) {
	s.enqueue(text, false)
}

func (s *Manager) enqueue(text string, projectUserMessage bool) {
	s.mu.Lock()
	s.enqueueLocked(text, projectUserMessage)
	s.mu.Unlock()
}

func (s *Manager) enqueueLocked(text string, projectUserMessage bool) {
	s.inputs = append(s.inputs, managerInput{projectUserMessage: projectUserMessage})
	s.pending++
	s.loop.Enqueue(agent.UserMessage{Content: text})
}

func (s *Manager) turnDone() {
	s.mu.Lock()
	s.pending--
	idle := false
	if s.pending <= 0 {
		s.pending = 0
		idle = !s.taskRunning
		if idle {
			s.settling = true
		}
	}
	s.mu.Unlock()
	if !idle {
		return
	}
	s.logSnapshot()
	s.mu.Lock()
	s.settling = false
	s.idle.Broadcast()
	s.mu.Unlock()
}

func (s *Manager) logSnapshot() {
	if s.logger == nil {
		return
	}
	snapshot := s.Metrics()
	s.logger.Info("runtime snapshot",
		"uptime", snapshot.Uptime,
		"pending", snapshot.Pending,
		"task_running", snapshot.TaskRunning,
		"tasks_total", snapshot.Tasks.Total,
		"tasks_active", snapshot.Tasks.Active,
		"tasks_done", snapshot.Tasks.Done,
		"tasks_failed", snapshot.Tasks.Failed,
		"tasks_canceled", snapshot.Tasks.Canceled,
		"model_completed", snapshot.Events.Model.Completed,
		"model_errors", snapshot.Events.Model.Errors,
		"model_active", snapshot.Events.Model.Active,
		"model_p50", snapshot.Events.Model.Duration.P50,
		"model_p95", snapshot.Events.Model.Duration.P95,
		"model_max", snapshot.Events.Model.Duration.Max,
		"model_ttft_p50", snapshot.Events.Model.TTFT.P50,
		"model_ttft_p95", snapshot.Events.Model.TTFT.P95,
		"model_ttft_max", snapshot.Events.Model.TTFT.Max,
		"model_delta_chunks", snapshot.Events.DeltaChunks,
		"model_delta_bytes", snapshot.Events.DeltaBytes,
		"model_stream_chunks", snapshot.Events.StreamChunks,
		"model_stream_idle", snapshot.Events.ModelStreamIdle,
		"model_retries", snapshot.Events.ModelRetries,
		"tokens", snapshot.Events.Tokens,
		"input_tokens", snapshot.Events.InputTokens,
		"cached_tokens", snapshot.Events.CachedTokens,
		"cache_write_tokens", snapshot.Events.CacheWriteTokens,
		"cache_hit_rate", snapshot.Events.CacheHitRate,
		"total_cache_hit_rate", snapshot.Events.TotalCacheHitRate,
		"total_tokens", snapshot.Events.Tokens+snapshot.Events.MemoryTokens,
		"tool_completed", snapshot.Events.Tool.Completed,
		"tool_errors", snapshot.Events.Tool.Errors,
		"tool_active", snapshot.Events.Tool.Active,
		"tool_p50", snapshot.Events.Tool.Duration.P50,
		"tool_p95", snapshot.Events.Tool.Duration.P95,
		"tool_max", snapshot.Events.Tool.Duration.Max,
		"task_p50", snapshot.Events.Task.Duration.P50,
		"task_p95", snapshot.Events.Task.Duration.P95,
		"task_max", snapshot.Events.Task.Duration.Max,
		"memory_ops_completed", snapshot.Events.Memory.Completed,
		"memory_ops_errors", snapshot.Events.Memory.Errors,
		"memory_ops_active", snapshot.Events.Memory.Active,
		"memory_ops_tokens", snapshot.Events.MemoryTokens,
		"memory_input_tokens", snapshot.Events.MemoryInputTokens,
		"memory_cached_tokens", snapshot.Events.MemoryCachedTokens,
		"memory_cache_write_tokens", snapshot.Events.MemoryCacheWriteTokens,
		"memory_ops_retries", snapshot.Events.MemoryRetries,
		"memory_stream_chunks", snapshot.Events.MemoryStreamChunks,
		"memory_stream_idle", snapshot.Events.MemoryStreamIdle,
		"memory_ttft_p50", snapshot.Events.Memory.TTFT.P50,
		"memory_ttft_p95", snapshot.Events.Memory.TTFT.P95,
		"memory_ttft_max", snapshot.Events.Memory.TTFT.Max,
		"memory_organizer_runs", snapshot.Events.MemoryOrganizerRuns,
		"memory_organizer_candidates", snapshot.Events.MemoryOrganizerCandidates,
		"memory_organizer_selected", snapshot.Events.MemoryOrganizerSelected,
		"memory_organizer_tokens", snapshot.Events.MemoryOrganizerTokens,
		"memory_organizer_duration", snapshot.Events.MemoryOrganizerDuration.Total,
		"memory_organizer_p50", snapshot.Events.MemoryOrganizerDuration.P50,
		"memory_organizer_p95", snapshot.Events.MemoryOrganizerDuration.P95,
		"memory_organizer_max", snapshot.Events.MemoryOrganizerDuration.Max,
		"memory_ops_p50", snapshot.Events.Memory.Duration.P50,
		"memory_ops_p95", snapshot.Events.Memory.Duration.P95,
		"memory_ops_max", snapshot.Events.Memory.Duration.Max,
		"exec_capacity", snapshot.Exec.Capacity,
		"exec_sandbox_backend", snapshot.Exec.SandboxBackend,
		"exec_network_isolation", snapshot.Exec.NetworkIsolation,
		"exec_queued", snapshot.Exec.Queued,
		"exec_active", snapshot.Exec.Active,
		"exec_peak_queued", snapshot.Exec.PeakQueued,
		"exec_peak_active", snapshot.Exec.PeakActive,
		"exec_requests", snapshot.Exec.Requests,
		"exec_started", snapshot.Exec.Started,
		"exec_completed", snapshot.Exec.Completed,
		"exec_errors", snapshot.Exec.Errors,
		"exec_canceled", snapshot.Exec.Canceled,
		"exec_timed_out", snapshot.Exec.TimedOut,
		"exec_wait_duration", snapshot.Exec.WaitDuration,
		"exec_run_duration", snapshot.Exec.RunDuration,
		"exec_tracked_process_groups", snapshot.Exec.TrackedProcessGroups,
		"exec_runtime_dirs", snapshot.Exec.RuntimeDirs,
		"exec_runtime_cleanup_errors", snapshot.Exec.RuntimeCleanupErrors,
		"exec_last_runtime_cleanup_error", snapshot.Exec.LastRuntimeCleanupError,
		"exec_heavy_capacity", snapshot.Exec.HeavyCapacity,
		"exec_heavy_queued", snapshot.Exec.HeavyQueued,
		"exec_heavy_active", snapshot.Exec.HeavyActive,
		"exec_heavy_peak_queued", snapshot.Exec.HeavyPeakQueued,
		"exec_heavy_peak_active", snapshot.Exec.HeavyPeakActive,
		"exec_heavy_wait_duration", snapshot.Exec.HeavyWaitDuration,
		"vfs_environments", snapshot.VFS.Environments,
		"vfs_live_dirs", snapshot.VFS.LiveDirs,
		"vfs_overlay_files", snapshot.VFS.OverlayFiles,
		"vfs_tombstones", snapshot.VFS.Tombstones,
		"vfs_overlay_bytes", snapshot.VFS.OverlayBytes,
		"vfs_materialize_copies", snapshot.VFS.MaterializeCopies,
		"vfs_materialize_copy_errors", snapshot.VFS.MaterializeCopyErrors,
		"vfs_materialize_copy_duration", snapshot.VFS.MaterializeCopyDuration,
		"vfs_handoffs", snapshot.VFS.Handoffs,
		"memory_environments", snapshot.Memory.Environments,
		"memory_baselines", snapshot.Memory.Baselines,
		"memory_subgraphs", snapshot.Memory.Subgraphs,
		"memory_nodes", snapshot.Memory.Nodes,
		"memory_edges", snapshot.Memory.Edges,
		"goroutines", snapshot.Runtime.Goroutines,
		"heap_alloc", snapshot.Runtime.HeapAlloc,
		"heap_objects", snapshot.Runtime.HeapObjects,
		"gc_count", snapshot.Runtime.GCCount,
		"gc_pause_total", snapshot.Runtime.GCPauseTotal,
	)
}

func (s *Manager) monitorSnapshots(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if s.Busy() {
				s.logSnapshot()
			}
		}
	}
}

func (s *Manager) setErr(err error) {
	s.mu.Lock()
	if err == nil || errors.Is(err, context.Canceled) {
		s.pending = 0
		s.idle.Broadcast()
		s.mu.Unlock()
		return
	}
	if s.err != nil {
		s.pending = 0
		s.idle.Broadcast()
		s.mu.Unlock()
		return
	}
	s.err = err
	s.pending = 0
	s.settling = true
	s.mu.Unlock()
	s.logSnapshot()
	s.mu.Lock()
	s.settling = false
	s.idle.Broadcast()
	s.mu.Unlock()
}

func (s *Manager) onEvent(_ context.Context, ev event.RuntimeEvent) {
	if ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Tokens > 0 {
		s.tokens.add(ev.AgentID, ev.Tokens)
	}
}

func formatReport(task coordination.Task, output string, err error, took time.Duration, tokens int) string {
	body := output
	label := "verifier 输出"
	if err != nil {
		body = err.Error()
		label = "流程错误"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[任务报告] %s · %s · 耗时 %s\n", task.ID, task.Outcome, took.Truncate(time.Second))
	fmt.Fprintf(&b, "目标: %s\n", task.Info)
	if tokens > 0 {
		fmt.Fprintf(&b, "token: %d\n", tokens)
	}
	fmt.Fprintf(&b, "%s:\n%s", label, body)
	return b.String()
}

type tokenCounter struct {
	mu      sync.Mutex
	byAgent map[string]int
}

func newTokenCounter() *tokenCounter {
	return &tokenCounter{byAgent: make(map[string]int)}
}

func (c *tokenCounter) add(agentID string, n int) {
	c.mu.Lock()
	c.byAgent[agentID] += n
	c.mu.Unlock()
}

func (c *tokenCounter) sumPrefix(prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for id, n := range c.byAgent {
		if strings.HasPrefix(id, prefix) {
			total += n
		}
	}
	return total
}
