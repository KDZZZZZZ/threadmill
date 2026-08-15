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

// AssembleRequestHook 在基础请求组装之后、调用模型之前改写请求。
type AssembleRequestHook func(context.Context, Request) (Request, error)

// AfterAssistantHook 在助手消息写入历史之后、执行工具之前运行。
type AfterAssistantHook func(context.Context, AssistantMessage) error

// CommitTurnHook 在本轮 ReAct 结束、AfterTurn 之前运行。
type CommitTurnHook func(context.Context) error

// TurnResult 描述一条用户消息对应的 ReAct 迭代结果。
type TurnResult struct {
	Err       error
	Preempted bool
	Steps     int
}

// Hooks 保存按注册顺序执行的生命周期钩子；每个阶段可注册多个钩子。
type Hooks struct {
	BeforeRun       []RunHook
	AfterRun        []AfterRunHook
	BeforeTurn      []TurnHook
	BeforeModel     []BeforeModelHook
	AfterModel      []AfterModelHook
	BeforeTool      []BeforeToolHook
	AfterTool       []AfterToolHook
	AfterTurn       []AfterTurnHook
	OnPreempt       []TurnHook
	AssembleRequest []AssembleRequestHook
	AfterAssistant  []AfterAssistantHook
	CommitTurn      []CommitTurnHook
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

// assembleRequest 按注册顺序改写模型请求，遇到首个错误即停止。
func (hooks Hooks) assembleRequest(ctx context.Context, request Request) (Request, error) {
	var err error
	for _, hook := range hooks.AssembleRequest {
		request, err = hook(ctx, request)
		if err != nil {
			return Request{}, newLifecycleError("assemble request", err)
		}
	}
	return request, nil
}

// afterAssistant 执行助手消息写入后的钩子，并隔离消息副本。
func (hooks Hooks) afterAssistant(ctx context.Context, message AssistantMessage) error {
	for _, hook := range hooks.AfterAssistant {
		if err := hook(ctx, cloneAssistantMessage(message)); err != nil {
			return newLifecycleError("after assistant", err)
		}
	}
	return nil
}

// commitTurn 执行全部轮次提交钩子，并逐步累积错误。
func (hooks Hooks) commitTurn(ctx context.Context) error {
	var hookErr error
	for _, hook := range hooks.CommitTurn {
		hookErr = errors.Join(hookErr, hook(ctx))
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
	return mergeHooks(Hooks{}, hooks)
}

// mergeHooks 把 extra 追加到 base 之后，并复制切片。
func mergeHooks(base, extra Hooks) Hooks {
	return Hooks{
		BeforeRun:       append(append([]RunHook(nil), base.BeforeRun...), extra.BeforeRun...),
		AfterRun:        append(append([]AfterRunHook(nil), base.AfterRun...), extra.AfterRun...),
		BeforeTurn:      append(append([]TurnHook(nil), base.BeforeTurn...), extra.BeforeTurn...),
		BeforeModel:     append(append([]BeforeModelHook(nil), base.BeforeModel...), extra.BeforeModel...),
		AfterModel:      append(append([]AfterModelHook(nil), base.AfterModel...), extra.AfterModel...),
		BeforeTool:      append(append([]BeforeToolHook(nil), base.BeforeTool...), extra.BeforeTool...),
		AfterTool:       append(append([]AfterToolHook(nil), base.AfterTool...), extra.AfterTool...),
		AfterTurn:       append(append([]AfterTurnHook(nil), base.AfterTurn...), extra.AfterTurn...),
		OnPreempt:       append(append([]TurnHook(nil), base.OnPreempt...), extra.OnPreempt...),
		AssembleRequest: append(append([]AssembleRequestHook(nil), base.AssembleRequest...), extra.AssembleRequest...),
		AfterAssistant:  append(append([]AfterAssistantHook(nil), base.AfterAssistant...), extra.AfterAssistant...),
		CommitTurn:      append(append([]CommitTurnHook(nil), base.CommitTurn...), extra.CommitTurn...),
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
	for _, hook := range hooks.AssembleRequest {
		if hook == nil {
			return fmt.Errorf("%w: assemble request", ErrNilHook)
		}
	}
	for _, hook := range hooks.AfterAssistant {
		if hook == nil {
			return fmt.Errorf("%w: after assistant", ErrNilHook)
		}
	}
	for _, hook := range hooks.CommitTurn {
		if hook == nil {
			return fmt.Errorf("%w: commit turn", ErrNilHook)
		}
	}
	return nil
}
