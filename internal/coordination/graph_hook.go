package coordination

import (
	"context"
	"fmt"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

// InjectCoordinationGraph 在每次调用模型前把最新协调图快照的稳定投影注入请求状态块。
func InjectCoordinationGraph(graph *Graph) agent.Hooks {
	return agent.Hooks{
		AssembleRequest: []agent.AssembleRequestHook{
			func(ctx context.Context, request agent.Request) (agent.Request, error) {
				if err := ctx.Err(); err != nil {
					return agent.Request{}, err
				}
				if graph == nil {
					return request, fmt.Errorf("inject coordination graph: nil graph")
				}
				snap, err := graph.SnapshotAt(ctx, 0)
				if err != nil {
					return request, err
				}
				payload, err := snap.PromptProjection()
				if err != nil {
					return request, fmt.Errorf("encode coordination graph: %w", err)
				}
				extra := "当前协调图（JSON：tasks 含 ID/Info/Outcome/RunPolicy 与角色节点，edges 含 From/To/Kind）。" +
					"root 按 tasks 出现顺序串行执行，RunPolicy=held 的 root 留在队列里不启动并挡住其后全部 root；唯一的并行面是 task 内部的辅助任务：\n" + string(payload)
				request.SetBlock("coordination", extra)
				return request, nil
			},
		},
	}
}
