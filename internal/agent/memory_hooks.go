package agent

import "context"

// InjectSubscribedMemory 把当前订阅子图里的节点拼进系统提示词。
func InjectSubscribedMemory(loop *Loop) AssembleRequestHook {
	return func(_ context.Context, request Request) (Request, error) {
		loop.mu.Lock()
		// ponytail: clones the whole graph; copy only subscribed nodes if snapshots get large
		graph := loop.refreshGraphCopyLocked().Graph
		subscribed := append([]string(nil), loop.subscribedSubgraphs...)
		loop.mu.Unlock()
		request.SystemPrompt = assembleSystemPrompt(
			request.SystemPrompt,
			graph,
			subscribed,
		)
		return request, nil
	}
}

// CompactOnOverflow 在用量超过上下文窗口时把旧消息整理进订阅子图。
func CompactOnOverflow(loop *Loop) AfterAssistantHook {
	return func(ctx context.Context, message AssistantMessage) error {
		return loop.compactIfNeeded(ctx, message.Usage)
	}
}

// CommitTailOnTurnEnd 在本轮结束时把剩余消息全部写入记忆图。
func CommitTailOnTurnEnd(loop *Loop) CommitTurnHook {
	return func(ctx context.Context) error {
		return loop.commitTail(ctx)
	}
}

// MemoryHooks 挂上默认的记忆注入、溢出压缩和退出整理。
func MemoryHooks(loop *Loop) Hooks {
	return Hooks{
		AssembleRequest: []AssembleRequestHook{InjectSubscribedMemory(loop)},
		AfterAssistant:  []AfterAssistantHook{CompactOnOverflow(loop)},
		CommitTurn:      []CommitTurnHook{CommitTailOnTurnEnd(loop)},
	}
}
