// Package session 把经理循环、协调图调度和任务报告串成一次可唤醒的会话。
package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// Options 打开会话所需的工作区、配置和可选依赖。
type Options struct {
	Root     string
	File     provider.FileConfig
	Provider agent.Provider
	Output   func(string)
	OnEvent  event.Handler
}

// Session 是长命经理加串行任务调度。
type Session struct {
	graph     *coordination.Graph
	stores    coordination.Stores
	assemble  coordination.AssembleFunc
	manager   *agent.Loop
	tokens    *tokenCounter
	output    func(string)
	modelName string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	pending   int
	idle      *sync.Cond
	err       error
	cancelRun context.CancelFunc
}

// Open 接线存储、装配经理并启动常驻 Run。
func Open(parent context.Context, opt Options) (*Session, error) {
	if parent == nil {
		panic("nil context")
	}
	if opt.Root == "" {
		return nil, fmt.Errorf("session: root is required")
	}
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

	reactDir := filepath.Join(opt.Root, ".threadmill", "react")
	progressDir := filepath.Join(opt.Root, ".threadmill", "progress")
	checkpoints, err := agent.NewDirCheckpointStore(reactDir)
	if err != nil {
		return nil, err
	}
	progress, err := coordination.NewDirProgressStore(progressDir)
	if err != nil {
		return nil, err
	}

	s := &Session{
		graph:     coordination.New(),
		tokens:    newTokenCounter(),
		output:    opt.Output,
		modelName: file.LLM.Model,
	}
	s.idle = sync.NewCond(&s.mu)
	s.stores = coordination.Stores{
		Memory: ctxgraph.NewStore(),
		Files:  vfs.NewStore(opt.Root),
		Exec: tmexec.New(tmexec.Config{
			Slots:       file.Exec.Slots,
			Timeout:     time.Duration(file.Exec.Timeout) * time.Second,
			OutputCapKB: file.Exec.OutputCapKB,
		}),
	}
	s.graph.SetProgressStore(progress)

	bus := event.NewBus(s.onEvent, opt.OnEvent)
	overlay := agent.FileOverlay{
		Tools:   file.Tools,
		Prompts: file.Prompts,
		Events:  bus,
	}
	s.assemble = coordination.Assemble(
		s.stores,
		llm,
		file.Agents,
		nil,
		file.LLM.ContextWindow,
		checkpoints,
		overlay,
	)
	manager, err := coordination.NewManagerLoop(
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
	if err := manager.AddHooks(s.hooks()); err != nil {
		return nil, err
	}
	s.manager = manager

	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	ready := make(chan struct{})
	if err := manager.AddHooks(agent.Hooks{
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
		err := manager.Run(ctx)
		s.setErr(err)
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		cancel()
		s.wg.Wait()
		return nil, ctx.Err()
	}
	return s, nil
}

// Send 把用户消息入队，唤醒经理。
func (s *Session) Send(text string) {
	s.addPending(1)
	s.manager.Enqueue(agent.UserMessage{Content: text})
}

// WaitIdle 等到经理队列清空且没有正在跑的任务。
func (s *Session) WaitIdle(ctx context.Context) error {
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
	for s.pending > 0 && s.err == nil && ctx.Err() == nil {
		s.idle.Wait()
	}
	if s.err != nil && !errors.Is(s.err, context.Canceled) {
		return s.err
	}
	return ctx.Err()
}

// Snapshot 返回当前协调图。
func (s *Session) Snapshot() coordination.Snapshot {
	return s.graph.Snapshot()
}

// Cancel 取消正在跑的任务树或抢占经理当前轮；没有可取消的工作时返回 false。
func (s *Session) Cancel() bool {
	s.mu.Lock()
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return s.manager.Preempt()
}

// ModelName 返回配置里的 LLM 模型名。
func (s *Session) ModelName() string {
	return s.modelName
}

// Busy 表示还有未完成的经理轮或任务。
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending > 0
}

// Close 停掉经理循环。
func (s *Session) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Session) hooks() agent.Hooks {
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

func (s *Session) runReady(ctx context.Context) error {
	var roots []coordination.Task
	for _, task := range s.graph.Snapshot().Tasks {
		if task.SpawnedFrom != "" || task.Outcome != coordination.OutcomeActive || task.RunPolicy != coordination.RunPolicyEnabled {
			continue
		}
		roots = append(roots, task)
	}
	var err error
	for _, task := range roots {
		err = errors.Join(err, s.runRoot(ctx, task))
	}
	return err
}

func (s *Session) runRoot(ctx context.Context, task coordination.Task) error {
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.cancelRun = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		if s.cancelRun != nil {
			s.cancelRun = nil
		}
		s.mu.Unlock()
	}()

	started := time.Now()
	before := s.tokens.sumPrefix(task.ID + ":")
	out, runErr := s.graph.Run(runCtx, task.ID, task.Info, s.stores, s.assemble)
	tokens := s.tokens.sumPrefix(task.ID+":") - before
	latest, ok := s.graph.Task(task.ID)
	if !ok {
		latest = task
	}
	s.enqueueReport(formatReport(latest, out, runErr, time.Since(started), tokens))
	return nil
}

func (s *Session) enqueueReport(text string) {
	if s.output != nil {
		s.output(text)
	}
	s.addPending(1)
	s.manager.Enqueue(agent.UserMessage{Content: text})
}

func (s *Session) addPending(n int) {
	s.mu.Lock()
	s.pending += n
	s.mu.Unlock()
}

func (s *Session) turnDone() {
	s.mu.Lock()
	s.pending--
	if s.pending <= 0 {
		s.pending = 0
		s.idle.Broadcast()
	}
	s.mu.Unlock()
}

func (s *Session) setErr(err error) {
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

func (s *Session) onEvent(_ context.Context, ev event.RuntimeEvent) {
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
