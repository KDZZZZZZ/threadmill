// Package manager 把经理循环、协调图调度和任务报告串成一个可唤醒的运行单元。
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// Options 启动 manager 所需的工作区、配置和可选依赖。
type Options struct {
	Root     string
	File     provider.FileConfig
	Provider agent.Provider
	Output   func(string)
	OnEvent  event.Handler
	Logger   *slog.Logger
}

// Manager 是长命经理加串行任务调度。
type Manager struct {
	graph       *coordination.Graph
	stores      coordination.Stores
	assemble    coordination.AssembleFunc
	loop        *agent.Loop
	tokens      *tokenCounter
	output      func(string)
	modelName   string
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	mu          sync.Mutex
	pending     int
	idle        *sync.Cond
	err         error
	cancelRun   context.CancelFunc
	taskRunning bool
	logFile     io.Closer
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
		loaded, err := provider.LoadConfig(opt.Root)
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

	s := &Manager{
		graph:     graph,
		tokens:    newTokenCounter(),
		output:    opt.Output,
		modelName: file.LLM.Model,
	}
	s.idle = sync.NewCond(&s.mu)
	s.stores = coordination.Stores{
		Memory: memory,
		Files:  vfs.NewStore(opt.Root),
		Exec: tmexec.New(tmexec.Config{
			Slots:       file.Exec.Slots,
			Timeout:     time.Duration(file.Exec.Timeout) * time.Second,
			OutputCapKB: file.Exec.OutputCapKB,
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
	bus := event.NewBus(s.onEvent, event.Monitor(logger), opt.OnEvent)
	overlay := agent.FileOverlay{
		Tools:   file.Tools,
		Prompts: file.Prompts,
		Events:  bus,
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
	if err := s.runReady(ctx); err != nil {
		cancel()
		s.wg.Wait()
		return nil, err
	}
	return s, nil
}

// Send 把用户消息入队，唤醒经理。
func (s *Manager) Send(text string) {
	if err := s.stores.ProjectManagerUserMessage(text); err != nil {
		s.setErr(err)
		return
	}
	s.addPending(1)
	s.loop.Enqueue(agent.UserMessage{Content: text})
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
	for (s.pending > 0 || s.taskRunning) && s.err == nil && ctx.Err() == nil {
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
	return s.pending > 0 || s.taskRunning
}

// Close 停掉经理循环。
func (s *Manager) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

func (s *Manager) hooks() agent.Hooks {
	return agent.Hooks{
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
			func(ctx context.Context, _ agent.UserMessage, _ agent.TurnResult) error {
				err := s.runReady(ctx)
				s.turnDone()
				return err
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
		if task.SpawnedFrom != "" || task.Outcome != coordination.OutcomeActive || task.RunPolicy != coordination.RunPolicyEnabled {
			continue
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
	if latest.Outcome == coordination.OutcomeActive || !reported {
		s.mu.Lock()
		s.taskRunning = false
		s.cancelRun = nil
		s.mu.Unlock()
		s.setErr(runErr)
		return
	}

	s.mu.Lock()
	s.taskRunning = false
	s.cancelRun = nil
	s.pending++
	s.mu.Unlock()
	if s.output != nil {
		s.output(report)
	}
	s.loop.Enqueue(agent.UserMessage{Content: report})
}

func (s *Manager) enqueueManager(text string) {
	s.addPending(1)
	s.loop.Enqueue(agent.UserMessage{Content: text})
}

func (s *Manager) addPending(n int) {
	s.mu.Lock()
	s.pending += n
	s.mu.Unlock()
}

func (s *Manager) turnDone() {
	s.mu.Lock()
	s.pending--
	if s.pending <= 0 {
		s.pending = 0
		s.idle.Broadcast()
	}
	s.mu.Unlock()
}

func (s *Manager) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil || errors.Is(err, context.Canceled) {
		s.pending = 0
		s.idle.Broadcast()
		return
	}
	if s.err == nil {
		s.err = err
	}
	s.pending = 0
	s.idle.Broadcast()
}

func (s *Manager) onEvent(_ context.Context, ev event.RuntimeEvent) {
	if ev.Kind == event.KindModel && ev.Phase == event.PhaseEnd && ev.Tokens > 0 {
		s.tokens.add(ev.AgentID, ev.Tokens)
	}
}

func formatReport(task coordination.Task, output string, err error, took time.Duration, tokens int) string {
	body := output
	if err != nil {
		body = err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[任务报告] %s · %s · 耗时 %s\n", task.ID, task.Outcome, took.Truncate(time.Second))
	fmt.Fprintf(&b, "目标: %s\n", task.Info)
	if tokens > 0 {
		fmt.Fprintf(&b, "token: %d\n", tokens)
	}
	fmt.Fprintf(&b, "verifier 输出:\n%s", body)
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
