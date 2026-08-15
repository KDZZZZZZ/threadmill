// Package agent 实现可抢占、可排队的 ReAct 运行循环。
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

const defaultMaxSteps = 512

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

// Provider 生成下一条助手消息；具体实现负责 LLM 协议与鉴权。
type Provider interface {
	Generate(ctx context.Context, request Request) (AssistantMessage, error)
}

// Config 配置 LLM Provider、工具、生命周期钩子和单次迭代上限。
type Config struct {
	Provider Provider
	Tools    []agenttool.Tool
	Hooks    Hooks
	MaxSteps int
}

// Loop 串行执行 ReAct 迭代，其公开方法可安全地并发调用。
type Loop struct {
	agentConfig agentConfig
	provider    Provider
	tools       map[string]agenttool.Tool
	definitions []agenttool.Definition
	hooks       Hooks
	maxSteps    int
	wake        chan struct{}

	mu              sync.Mutex
	queue           []UserMessage
	messages        []Message
	usedToolCallIDs map[string]struct{}
	running         bool
	turnCancel      context.CancelFunc
	turnPreempted   bool
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

	tools, definitions, err := prepareTools(config.Tools)
	if err != nil {
		return nil, err
	}

	return &Loop{
		agentConfig:     newAgentConfig(),
		provider:        config.Provider,
		tools:           tools,
		definitions:     definitions,
		hooks:           cloneHooks(config.Hooks),
		maxSteps:        maxSteps,
		wake:            make(chan struct{}, 1),
		queue:           []UserMessage{},
		messages:        []Message{},
		usedToolCallIDs: make(map[string]struct{}),
	}, nil
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

	for {
		message, err := l.next(ctx)
		if err != nil {
			return err
		}
		if err := l.runTurn(ctx, message); err != nil {
			return err
		}
	}
}

// runTurn 对一条用户消息执行“模型生成—工具执行—结果回填”的 ReAct 循环。
func (l *Loop) runTurn(ctx context.Context, input UserMessage) error {
	turnCtx, cancel := context.WithCancel(ctx)
	l.startTurn(cancel)

	turnErr := l.hooks.beforeTurn(turnCtx, input)
	steps := 0
	completed := false
	if turnErr == nil {
		l.appendMessage(Message{Role: RoleUser, Content: input.Content})
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

			if err := l.validateToolCallIDs(response.ToolCalls); err != nil {
				turnErr = err
				break
			}
			l.appendMessage(messageFromAssistant(response))
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
		return hookErr
	}
	return errors.Join(turnErr, hookErr)
}

// generate 调用 Provider 并执行模型生命周期钩子。
func (l *Loop) generate(ctx context.Context) (AssistantMessage, error) {
	request := l.assembleRequest()
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
