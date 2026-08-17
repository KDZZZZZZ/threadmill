package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// InjectSubscribedMemory 把当前订阅子图里的节点拼进系统提示词。
func InjectSubscribedMemory(loop *Loop) AssembleRequestHook {
	return func(ctx context.Context, request Request) (Request, error) {
		out, err := loop.execHidden(ctx, injectSubscribedMemoryToolName, json.RawMessage(`{}`))
		if err != nil {
			return request, err
		}
		request.SystemPrompt = joinSystemPrompt(request.SystemPrompt, out.Content)
		return request, nil
	}
}

// CompactOnOverflow 在用量超过上下文窗口时把旧消息整理进订阅子图。
func CompactOnOverflow(loop *Loop) AfterAssistantHook {
	return func(ctx context.Context, message AssistantMessage) error {
		if !ShouldCompact(message.Usage, loop.contextWindow) {
			return nil
		}
		keep := keepRecentBudget(loop.contextWindow)
		return execHiddenErr(loop, ctx, compactMemoryToolName, keepRecentArgs(keep))
	}
}

// CommitTailOnTurnEnd 在本轮结束时把剩余消息全部写入记忆图。
func CommitTailOnTurnEnd(loop *Loop) CommitTurnHook {
	return func(ctx context.Context) error {
		return execHiddenErr(loop, ctx, compactMemoryToolName, keepRecentArgs(0))
	}
}

func execHiddenErr(loop *Loop, ctx context.Context, name string, args json.RawMessage) error {
	_, err := loop.execHidden(ctx, name, args)
	return err
}

func memoryHookSet(loop *Loop) Hooks {
	return Hooks{
		AssembleRequest: []AssembleRequestHook{InjectSubscribedMemory(loop)},
		AfterAssistant:  []AfterAssistantHook{CompactOnOverflow(loop)},
		CommitTurn:      []CommitTurnHook{CommitTailOnTurnEnd(loop)},
	}
}

func ensureHiddenMemoryTools(loop *Loop) error {
	if loop == nil {
		return nil
	}
	return registerTools(loop, hiddenMemoryTools())
}

// MemoryHooks 挂上默认的记忆注入、溢出压缩和退出整理，并注册对应的隐藏工具。
func MemoryHooks(loop *Loop) Hooks {
	if err := ensureHiddenMemoryTools(loop); err != nil {
		return Hooks{
			AssembleRequest: []AssembleRequestHook{
				func(_ context.Context, request Request) (Request, error) {
					return request, fmt.Errorf("install hidden memory tools: %w", err)
				},
			},
		}
	}
	return memoryHookSet(loop)
}
