package coordination

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

// InjectCoordinationGraph 在每次调用模型前把最新协调图快照拼进系统提示。
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
				payload, err := json.Marshal(snap)
				if err != nil {
					return request, fmt.Errorf("encode coordination graph: %w", err)
				}
				extra := "当前协调图（JSON：tasks[].id/info/outcome/sequence，edges[].from/to 为节点关联，helps 为拆分请求）：\n" + string(payload)
				if request.SystemPrompt == "" {
					request.SystemPrompt = extra
				} else {
					request.SystemPrompt += "\n\n" + extra
				}
				return request, nil
			},
		},
	}
}
