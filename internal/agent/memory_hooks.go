package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// InjectSubscribedMemory 把任务启动包注入稳定前缀，其他记忆投影注入请求尾部。
func InjectSubscribedMemory(loop *Loop) AssembleRequestHook {
	return func(ctx context.Context, request Request) (Request, error) {
		blocks, err := loop.subscribedMemoryBlocks(ctx)
		if err != nil {
			return request, err
		}
		request.SetStableBlock("memory", blocks.Stable)
		request.SetBlock("memory", blocks.Mutable)
		return request, nil
	}
}

// revisionPeek 由 *ctxgraph.EnvView 实现；不能窥探版本时按未缓存处理。
type revisionPeek interface {
	Revision() int64
}

// subscribedMemoryBlock 返回当前订阅的记忆投影文本。
// 图 revision 与订阅列表都未变时直接复用上一次的字符串，跳过隐藏工具执行，
// organizer 提交无关边/元数据之外的情况也不会产生新投影字节。
// ponytail: revision 只是失效提示，多算属于误报且无害；只有绕过 Store 的显式 API、
// 以相同 revision 提交节点变化才可能读到旧文本（生产写路径都会递增 revision）。
func (l *Loop) subscribedMemoryBlocks(ctx context.Context) (memoryProjection, error) {
	l.mu.Lock()
	stable := uniqueIDs(l.stableSubscribedSubgraphs)
	mutable := append([]string(nil), l.fixedSubscribedSubgraphs...)
	mutable = uniqueIDs(append(mutable, l.subscribedSubgraphs...))
	subs := uniqueIDs(append(append([]string(nil), stable...), mutable...))
	cachedRev := l.memoryBlockRev
	cachedSubs := l.memoryBlockSubs
	cachedStableText := l.memoryStableBlockText
	cachedText := l.memoryBlockText
	memory := l.memory
	l.mu.Unlock()

	if peek, ok := memory.(revisionPeek); ok &&
		cachedSubs != nil && sameStrings(cachedSubs, subs) && peek.Revision() == cachedRev {
		return memoryProjection{Stable: cachedStableText, Mutable: cachedText}, nil
	}

	out, err := l.execHidden(ctx, injectSubscribedMemoryToolName, json.RawMessage(`{}`))
	if err != nil {
		return memoryProjection{}, err
	}
	projection := memoryProjection{Mutable: out.Content}
	if len(out.Details) > 0 {
		if err := json.Unmarshal(out.Details, &projection); err != nil {
			return memoryProjection{}, fmt.Errorf("decode memory projection: %w", err)
		}
	}
	rev := int64(-1)
	if peek, ok := memory.(revisionPeek); ok {
		rev = peek.Revision()
	}
	l.mu.Lock()
	l.memoryBlockRev = rev
	l.memoryBlockSubs = subs
	l.memoryStableBlockText = projection.Stable
	l.memoryBlockText = projection.Mutable
	l.mu.Unlock()
	return projection, nil
}

// subscribedMemoryBlock 保留合并投影视图，供无需区分缓存层的调用方使用。
func (l *Loop) subscribedMemoryBlock(ctx context.Context) (string, error) {
	blocks, err := l.subscribedMemoryBlocks(ctx)
	if err != nil {
		return "", err
	}
	return joinMemoryBlocks(blocks.Stable, blocks.Mutable), nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CompactOnOverflow 在用量接近上下文窗口时把旧消息整理进记忆图；
// 整理后按 memory.curation 阈值触发全图深度整理。
func CompactOnOverflow(loop *Loop) AfterAssistantHook {
	return func(ctx context.Context, message AssistantMessage) error {
		if !ShouldCompact(message.Usage, loop.contextWindow) {
			return nil
		}
		keep := keepRecentBudget(loop.contextWindow)
		return compactAndMaybeCurate(ctx, loop, keep)
	}
}

// CommitTailOnTurnEnd 在本轮结束时把剩余消息全部写入记忆图；
// 整理后按 memory.curation 阈值触发全图深度整理。
func CommitTailOnTurnEnd(loop *Loop) CommitTurnHook {
	return func(ctx context.Context) error {
		return compactAndMaybeCurate(ctx, loop, 0)
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
