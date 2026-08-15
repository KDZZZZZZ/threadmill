package agent

import (
	"context"
	"errors"
	"fmt"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// ErrNilHook 表示生命周期钩子列表包含 nil。
var ErrNilHook = errors.New("agent: nil lifecycle hook")

// lifecycleError 标识生命周期钩子自身的错误，避免抢占逻辑将其当作普通取消吞掉。
type lifecycleError struct {
	phase string
	err   error
}

func (e *lifecycleError) Error() string {
	return fmt.Sprintf("%s hook: %v", e.phase, e.err)
}

func (e *lifecycleError) Unwrap() error {
	return e.err
}

// RunHook 在循环启动前执行。
type RunHook func(context.Context) error

// AfterRunHook 在循环退出时执行，并接收当前累计错误。
type AfterRunHook func(context.Context, error) error

// TurnHook 在单条用户消息开始处理前或被抢占后执行。
type TurnHook func(context.Context, UserMessage) error

// BeforeModelHook 在每次调用模型前执行。
type BeforeModelHook func(context.Context, Request) error

// AfterModelHook 在每次模型调用结束后执行，包括模型返回错误的情况。
type AfterModelHook func(context.Context, Request, AssistantMessage, error) error

// BeforeToolHook 在每次执行已解析的工具调用前执行，返回错误可阻止执行。
type BeforeToolHook func(context.Context, agenttool.Call) error

// AfterToolHook 在每个工具调用产生结果后执行，包括失败和取消结果。
type AfterToolHook func(context.Context, agenttool.Call, agenttool.Result) error

// AfterTurnHook 在单条用户消息处理结束后执行。
type AfterTurnHook func(context.Context, UserMessage, TurnResult) error

// TurnResult 描述一条用户消息对应的 ReAct 迭代结果。
type TurnResult struct {
	Err       error
	Preempted bool
	Steps     int
}

// Hooks 保存按注册顺序执行的生命周期钩子；每个阶段可注册多个钩子。
type Hooks struct {
	BeforeRun   []RunHook
	AfterRun    []AfterRunHook
	BeforeTurn  []TurnHook
	BeforeModel []BeforeModelHook
	AfterModel  []AfterModelHook
	BeforeTool  []BeforeToolHook
	AfterTool   []AfterToolHook
	AfterTurn   []AfterTurnHook
	OnPreempt   []TurnHook
}

// beforeRun 按注册顺序执行循环前置钩子，遇到首个错误即停止。
func (hooks Hooks) beforeRun(ctx context.Context) error {
	for _, hook := range hooks.BeforeRun {
		if err := hook(ctx); err != nil {
			return err
		}
	}
	return nil
}

// afterRun 执行全部循环后置钩子，并逐步累积错误。
func (hooks Hooks) afterRun(ctx context.Context, runErr error) error {
	for _, hook := range hooks.AfterRun {
		runErr = errors.Join(runErr, hook(ctx, runErr))
	}
	return runErr
}

// beforeTurn 执行单轮前置钩子。
func (hooks Hooks) beforeTurn(ctx context.Context, message UserMessage) error {
	return runTurnHooks(ctx, message, hooks.BeforeTurn)
}

// onPreempt 执行抢占钩子。
func (hooks Hooks) onPreempt(ctx context.Context, message UserMessage) error {
	return runTurnHooks(ctx, message, hooks.OnPreempt)
}

// afterTurn 执行全部单轮后置钩子并合并错误。
func (hooks Hooks) afterTurn(ctx context.Context, message UserMessage, result TurnResult) error {
	var hookErr error
	for _, hook := range hooks.AfterTurn {
		hookErr = errors.Join(hookErr, hook(ctx, message, result))
	}
	return hookErr
}

// beforeModel 执行模型前置钩子，并隔离请求副本。
func (hooks Hooks) beforeModel(ctx context.Context, request Request) error {
	for _, hook := range hooks.BeforeModel {
		if err := hook(ctx, cloneRequest(request)); err != nil {
			return newLifecycleError("before model", err)
		}
	}
	return nil
}

// afterModel 执行全部模型后置钩子，并隔离请求及响应副本。
func (hooks Hooks) afterModel(
	ctx context.Context,
	request Request,
	response AssistantMessage,
	providerErr error,
) error {
	var hookErr error
	for _, hook := range hooks.AfterModel {
		if err := hook(
			ctx,
			cloneRequest(request),
			cloneAssistantMessage(response),
			providerErr,
		); err != nil {
			hookErr = errors.Join(hookErr, newLifecycleError("after model", err))
		}
	}
	return hookErr
}

// beforeTool 执行工具前置钩子，并隔离调用参数副本。
func (hooks Hooks) beforeTool(ctx context.Context, call agenttool.Call) error {
	for _, hook := range hooks.BeforeTool {
		if err := hook(ctx, cloneCall(call)); err != nil {
			return err
		}
	}
	return nil
}

// afterTool 即使上下文已取消也会调用全部工具后置钩子。
func (hooks Hooks) afterTool(
	ctx context.Context,
	call agenttool.Call,
	result agenttool.Result,
) error {
	var hookErr error
	for _, hook := range hooks.AfterTool {
		if err := hook(ctx, cloneCall(call), result); err != nil {
			hookErr = errors.Join(hookErr, newLifecycleError("after tool", err))
		}
	}
	return hookErr
}

// runTurnHooks 按注册顺序执行迭代钩子，遇到首个错误即停止。
func runTurnHooks(ctx context.Context, message UserMessage, hooks []TurnHook) error {
	for _, hook := range hooks {
		if err := hook(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

// cloneHooks 复制全部钩子切片，避免调用方后续修改配置产生数据竞争。
func cloneHooks(hooks Hooks) Hooks {
	return Hooks{
		BeforeRun:   append([]RunHook(nil), hooks.BeforeRun...),
		AfterRun:    append([]AfterRunHook(nil), hooks.AfterRun...),
		BeforeTurn:  append([]TurnHook(nil), hooks.BeforeTurn...),
		BeforeModel: append([]BeforeModelHook(nil), hooks.BeforeModel...),
		AfterModel:  append([]AfterModelHook(nil), hooks.AfterModel...),
		BeforeTool:  append([]BeforeToolHook(nil), hooks.BeforeTool...),
		AfterTool:   append([]AfterToolHook(nil), hooks.AfterTool...),
		AfterTurn:   append([]AfterTurnHook(nil), hooks.AfterTurn...),
		OnPreempt:   append([]TurnHook(nil), hooks.OnPreempt...),
	}
}

// newLifecycleError 为钩子错误补充生命周期阶段。
func newLifecycleError(phase string, err error) error {
	return &lifecycleError{phase: phase, err: err}
}

// hasLifecycleError 判断错误链中是否包含钩子自身的失败。
func hasLifecycleError(err error) bool {
	var target *lifecycleError
	return errors.As(err, &target)
}

// validateHooks 拒绝 nil 钩子，避免运行时空指针 panic。
func validateHooks(hooks Hooks) error {
	for _, hook := range hooks.BeforeRun {
		if hook == nil {
			return fmt.Errorf("%w: before run", ErrNilHook)
		}
	}
	for _, hook := range hooks.AfterRun {
		if hook == nil {
			return fmt.Errorf("%w: after run", ErrNilHook)
		}
	}
	for _, hook := range hooks.BeforeTurn {
		if hook == nil {
			return fmt.Errorf("%w: before turn", ErrNilHook)
		}
	}
	for _, hook := range hooks.BeforeModel {
		if hook == nil {
			return fmt.Errorf("%w: before model", ErrNilHook)
		}
	}
	for _, hook := range hooks.AfterModel {
		if hook == nil {
			return fmt.Errorf("%w: after model", ErrNilHook)
		}
	}
	for _, hook := range hooks.BeforeTool {
		if hook == nil {
			return fmt.Errorf("%w: before tool", ErrNilHook)
		}
	}
	for _, hook := range hooks.AfterTool {
		if hook == nil {
			return fmt.Errorf("%w: after tool", ErrNilHook)
		}
	}
	for _, hook := range hooks.AfterTurn {
		if hook == nil {
			return fmt.Errorf("%w: after turn", ErrNilHook)
		}
	}
	for _, hook := range hooks.OnPreempt {
		if hook == nil {
			return fmt.Errorf("%w: on preempt", ErrNilHook)
		}
	}
	return nil
}
