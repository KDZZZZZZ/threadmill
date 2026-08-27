// Package agent 实现可抢占、可排队的 ReAct 运行循环。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const defaultMaxSteps = 512

// DefaultSystemPrompt 是未配置 SystemPrompt 时 Loop 使用的系统提示词。
const DefaultSystemPrompt = `你是 Threadmill，一个通过 ReAct 循环完成任务的 AI Agent。

根据用户请求决定下一步行动。需要读取信息或执行操作时，调用可用工具；不要编造工具结果。收到工具结果后继续处理，直到可以直接回答用户。`

var nextAgentID atomic.Uint64

var (
	// ErrNilProvider 表示循环没有可调用的 LLM Provider。
	ErrNilProvider = errors.New("agent: nil provider")
	// ErrInvalidConfig 表示循环配置无效。
	ErrInvalidConfig = errors.New("agent: invalid config")
	// ErrRunActive 表示已有 Run 调用处于运行状态。
	ErrRunActive = errors.New("agent: loop is already running")
	// ErrMaxSteps 表示单次 ReAct 迭代超过了模型调用上限。
	ErrMaxSteps = errors.New("agent: maximum react steps exceeded")
)

// agentConfig 是 Loop 在 Run 前确定的静态 Agent 配置。
type agentConfig struct {
	systemPrompt string
}

func newAgentConfig() agentConfig {
	return agentConfig{systemPrompt: DefaultSystemPrompt}
}

// Provider 生成下一条助手消息；具体实现负责 LLM 协议与鉴权。
type Provider interface {
	Generate(ctx context.Context, request Request) (AssistantMessage, error)
}

// Config 配置 LLM Provider、工具、生命周期钩子、单次迭代上限、上下文窗口、可选的进行中 ReAct 快照，以及运行时事件总线。
type Config struct {
	AgentID         string
	Provider        Provider
	Tools           []agenttool.Tool
	Hooks           Hooks
	MaxSteps        int
	ContextWindow   int
	SystemPrompt    string
	CheckpointStore CheckpointStore
	Events          *event.Bus
}

// Loop 串行执行 ReAct 迭代，其公开方法可安全地并发调用。
type Loop struct {
	agentConfig   agentConfig
	provider      Provider
	tools         map[string]agenttool.Tool
	definitions   []agenttool.Definition
	hooks         Hooks
	maxSteps      int
	contextWindow int
	wake          chan struct{}
	checkpoints   CheckpointStore
	events        *event.Bus
	memory        env.MemoryView
	curation      CurationConfig

	mu                       sync.Mutex
	queue                    []UserMessage
	messages                 []Message
	usedToolCallIDs          map[string]struct{}
	subscribedSubgraphs      []string
	fixedSubscribedSubgraphs []string
	agentID                  string
	running                  bool
	compactPrompt            string
	compactJSONReminder      string
	dropContextReminder      string
	organizeQueryInstruction string
	reactCommitted           bool
	turnCancel               context.CancelFunc
	turnPreempted            bool
}

// NewLoop 校验并复制配置，创建一个尚未运行的 ReAct 循环。
func NewLoop(config Config) (*Loop, error) {
	if config.Provider == nil {
		return nil, ErrNilProvider
	}
	if err := validateHooks(config.Hooks); err != nil {
		return nil, err
	}

	maxSteps := config.MaxSteps
	if maxSteps == 0 {
		maxSteps = defaultMaxSteps
	}
	if maxSteps < 0 {
		return nil, fmt.Errorf("%w: max steps must not be negative", ErrInvalidConfig)
	}
	if config.ContextWindow < 0 {
		return nil, fmt.Errorf("%w: context window must not be negative", ErrInvalidConfig)
	}

	agentID := config.AgentID
	if agentID == "" {
		agentID = fmt.Sprintf("agent-%d", nextAgentID.Add(1))
	}

	cfg := newAgentConfig()
	if config.SystemPrompt != "" {
		cfg.systemPrompt = config.SystemPrompt
	}

	loop := &Loop{
		agentConfig:              cfg,
		hooks:                    cloneHooks(config.Hooks),
		maxSteps:                 maxSteps,
		contextWindow:            config.ContextWindow,
		wake:                     make(chan struct{}, 1),
		checkpoints:              config.CheckpointStore,
		events:                   config.Events,
		queue:                    []UserMessage{},
		messages:                 []Message{},
		usedToolCallIDs:          make(map[string]struct{}),
		subscribedSubgraphs:      []string{},
		fixedSubscribedSubgraphs: []string{},
		agentID:                  agentID,
	}
	loop.provider = eventProvider{inner: config.Provider, loop: loop}

	tools, definitions, err := prepareTools(config.Tools)
	if err != nil {
		return nil, err
	}
	loop.tools = tools
	loop.definitions = definitions
	bindRequesters(loop, tools)
	return loop, nil
}

// AddHooks 在已有生命周期钩子之后追加 extra；不得在 Run 期间调用。
func (l *Loop) AddHooks(extra Hooks) error {
	merged := mergeHooks(l.hooks, extra)
	if err := validateHooks(merged); err != nil {
		return err
	}
	l.hooks = merged
	return nil
}

// Ask 处理一条查询并返回最后一条助手回复；与 Run 互斥。
// 若存在未完成的 ReAct 快照，则忽略 query，从快照继续，直到该回合正常结束再扔掉快照。
func (l *Loop) Ask(ctx context.Context, query string) (string, error) {
	if ctx == nil {
		panic("nil context")
	}
	if !l.startRun() {
		return "", ErrRunActive
	}
	defer l.finishRun()

	restored, err := l.restoreCheckpoint()
	if err != nil {
		return "", err
	}
	if restored {
		return l.continueTurn(ctx)
	}
	return l.runTurn(ctx, UserMessage{Content: query})
}

// Enqueue 将用户消息追加到先进先出队列，并唤醒正在等待的 Run。
func (l *Loop) Enqueue(message UserMessage) {
	l.mu.Lock()
	l.queue = append(l.queue, message)
	l.mu.Unlock()

	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Preempt 协作式取消当前 ReAct 迭代，但不会停止 Run 或清空后续消息。
func (l *Loop) Preempt() bool {
	l.mu.Lock()
	cancel := l.turnCancel
	if cancel != nil {
		l.turnPreempted = true
	}
	l.mu.Unlock()

	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Run 持续处理排队消息，直到 ctx 被取消或模型及生命周期钩子失败。
// 同一时刻只允许一个 Run 调用处于运行状态。
func (l *Loop) Run(ctx context.Context) (runErr error) {
	if ctx == nil {
		panic("nil context")
	}
	if !l.startRun() {
		return ErrRunActive
	}
	defer func() {
		runErr = l.hooks.afterRun(ctx, runErr)
		l.finishRun()
	}()

	if err := l.hooks.beforeRun(ctx); err != nil {
		return err
	}
	restored, err := l.restoreCheckpoint()
	if err != nil {
		return err
	}
	if restored {
		if _, err := l.continueTurn(ctx); err != nil {
			return err
		}
	}

	for {
		message, err := l.next(ctx)
		if err != nil {
			return err
		}
		if _, err := l.runTurn(ctx, message); err != nil {
			return err
		}
	}
}

// runTurn 对一条用户消息执行“模型生成—工具执行—结果回填”的 ReAct 循环。
func (l *Loop) runTurn(ctx context.Context, input UserMessage) (string, error) {
	l.mu.Lock()
	l.reactCommitted = false
	l.mu.Unlock()
	return l.runReact(ctx, input, false)
}

func (l *Loop) continueTurn(ctx context.Context) (string, error) {
	if l.committed() {
		return l.finishCommittedTurn()
	}
	return l.runReact(ctx, checkpointUser(l.Messages()), true)
}

func (l *Loop) finishCommittedTurn() (string, error) {
	l.mu.Lock()
	answer := lastAssistantText(l.messages)
	l.mu.Unlock()
	return answer, l.discardReact()
}

func (l *Loop) runReact(ctx context.Context, input UserMessage, resume bool) (string, error) {
	turnCtx, cancel := context.WithCancel(ctx)
	l.startTurn(cancel)

	turnErr := l.hooks.beforeTurn(turnCtx, input)
	steps := 0
	completed := false
	if turnErr == nil && !resume {
		turnErr = l.appendMessage(Message{
			Role:      RoleUser,
			Content:   input.Content,
			Timestamp: input.Timestamp,
		})
	}
	if turnErr == nil && resume {
		if pending := unpairedToolCalls(l.Messages()); len(pending) > 0 {
			turnErr = l.executeToolCalls(turnCtx, pending)
		} else if reactComplete(l.Messages()) {
			completed = true
		}
	}
	if turnErr == nil && !completed {
		for steps < l.maxSteps {
			if err := turnCtx.Err(); err != nil {
				turnErr = err
				break
			}

			response, err := l.generate(turnCtx)
			steps++
			if err != nil {
				turnErr = fmt.Errorf("generating model response: %w", err)
				break
			}
			response = stampAssistant(response)

			if err := l.validateToolCallIDs(response.ToolCalls); err != nil {
				turnErr = err
				break
			}
			if err := l.appendMessage(messageFromAssistant(response)); err != nil {
				turnErr = err
				break
			}
			if err := l.hooks.afterAssistant(turnCtx, response); err != nil {
				turnErr = err
				break
			}
			if len(response.ToolCalls) == 0 {
				completed = true
				break
			}
			if err := l.executeToolCalls(turnCtx, response.ToolCalls); err != nil {
				turnErr = err
				break
			}
		}
	}
	if turnErr == nil && !completed {
		turnErr = ErrMaxSteps
	}

	l.mu.Lock()
	answer := lastAssistantText(l.messages)
	l.mu.Unlock()

	if completed {
		if !l.committed() {
			if err := l.hooks.commitTurn(ctx); err != nil {
				turnErr = errors.Join(turnErr, err)
			} else if err := l.markReactCommitted(); err != nil {
				turnErr = errors.Join(turnErr, err)
			}
		}
		if turnErr == nil {
			if err := l.discardReact(); err != nil {
				turnErr = errors.Join(turnErr, err)
			}
		}
	}
	preempted := l.finishTurn()
	if !preempted && ctx.Err() != nil && turnErr == nil {
		turnErr = ctx.Err()
	}

	result := TurnResult{
		Err:       turnErr,
		Preempted: preempted,
		Steps:     steps,
	}
	var hookErr error
	if preempted {
		hookErr = l.hooks.onPreempt(ctx, input)
	}
	hookErr = errors.Join(hookErr, l.hooks.afterTurn(ctx, input, result))

	if preempted && errors.Is(turnErr, context.Canceled) && !hasLifecycleError(turnErr) {
		return answer, hookErr
	}
	return answer, errors.Join(turnErr, hookErr)
}

// generate 调用 Provider 并执行模型生命周期钩子。
func (l *Loop) generate(ctx context.Context) (AssistantMessage, error) {
	request := l.assembleRequest()
	request, err := l.hooks.assembleRequest(ctx, request)
	if err != nil {
		return AssistantMessage{}, err
	}
	if err := l.hooks.beforeModel(ctx, request); err != nil {
		return AssistantMessage{}, err
	}
	if err := ctx.Err(); err != nil {
		return AssistantMessage{}, err
	}

	response, providerErr := l.provider.Generate(ctx, cloneRequest(request))
	hookErr := l.hooks.afterModel(ctx, request, response, providerErr)
	return response, errors.Join(providerErr, hookErr)
}

// next 从队首取消息；队列为空时等待入队通知或上下文取消。
func (l *Loop) next(ctx context.Context) (UserMessage, error) {
	for {
		if err := ctx.Err(); err != nil {
			return UserMessage{}, err
		}

		l.mu.Lock()
		if len(l.queue) > 0 {
			message := l.queue[0]
			l.queue[0] = UserMessage{}
			l.queue = l.queue[1:]
			l.mu.Unlock()
			return message, nil
		}
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return UserMessage{}, ctx.Err()
		case <-l.wake:
		}
	}
}

// startRun 将循环标记为运行中；已有 Run 活跃时返回 false。
func (l *Loop) startRun() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running {
		return false
	}
	l.running = true
	return true
}

// finishRun 清除循环运行状态，使后续 Run 可以启动。
func (l *Loop) finishRun() {
	l.mu.Lock()
	l.running = false
	l.mu.Unlock()
}

// startTurn 记录当前迭代的取消函数，使 Preempt 可以取消它。
func (l *Loop) startTurn(cancel context.CancelFunc) {
	l.mu.Lock()
	l.turnCancel = cancel
	l.turnPreempted = false
	l.mu.Unlock()
}

// finishTurn 清除当前迭代状态，并返回它是否曾被抢占。
func (l *Loop) finishTurn() bool {
	l.mu.Lock()
	cancel := l.turnCancel
	preempted := l.turnPreempted
	l.turnCancel = nil
	l.turnPreempted = false
	l.mu.Unlock()

	cancel()
	return preempted
}

// BindEvents 设置运行时事件总线；应在 Run 之前调用。
func (l *Loop) BindEvents(bus *event.Bus) {
	if l == nil {
		return
	}
	l.events = bus
}

type eventProvider struct {
	inner Provider
	loop  *Loop
}

func (p eventProvider) Generate(ctx context.Context, request Request) (AssistantMessage, error) {
	retries := 0
	ctx = event.WithRetrySink(ctx, func(reason string) {
		retries++
		p.loop.publish(ctx, event.ModelRetry(p.loop.agentID, retries, reason))
	})
	ctx = event.WithDeltaActivitySink(ctx, func(text bool) {
		activity := event.ModelDelta(p.loop.agentID, "")
		activity.StreamText = text
		p.loop.publish(ctx, activity)
	})
	if p.loop.agentID != "manager" {
		ctx = event.WithReplayableDeltas(ctx)
	}
	ctx = event.WithDeltaSink(ctx, func(delta string) {
		p.loop.publish(ctx, event.ModelDelta(p.loop.agentID, delta))
	})
	p.loop.publish(ctx, event.ModelStart(p.loop.agentID, len(request.Messages), len(request.Tools)))
	started := time.Now()
	message, err := p.inner.Generate(ctx, request)
	tokens := 0
	cachedTokens := 0
	if message.Usage != nil {
		tokens = message.Usage.TotalTokens
		cachedTokens = message.Usage.CachedTokens
	}
	end := event.ModelEnd(
		p.loop.agentID,
		message.Model,
		started,
		len(message.ToolCalls),
		tokens,
		cachedTokens,
		err,
	)
	end.Retries = retries
	p.loop.publish(ctx, end)
	return message, err
}

func (l *Loop) publish(ctx context.Context, ev event.RuntimeEvent) {
	if l == nil {
		return
	}
	l.events.Publish(ctx, ev)
}
