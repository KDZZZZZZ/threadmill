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
				payload, err := graph.Snapshot().PromptProjection()
				if err != nil {
					return request, fmt.Errorf("encode coordination graph: %w", err)
				}
				extra := "当前协调图（JSON：tasks 含 ID/Info/Outcome/RunPolicy/Persistent/RealDirectory/Activation 与角色节点，edges 只有 from/to）。" +
					"project_task_id 标识真实目录归属；project_messages 是运行中的显式交流记录，不是用户授权或验收结论。" +
					"enabled 的 active task 可独立运行；held 只暂停自身，其显式下游等待对应输出。" +
					"普通边传递固定的完整文件与记忆状态：共同部分直接取，先处理文件差异，再整理记忆差异；单输入直接继承。" +
					"task 可以没有外部出边；persistent task 完成本次 activation 后进入 idle，由 continue_task 或 close_task 继续管理：\n" + string(payload)
				request.SetBlock("coordination", extra)
				return request, nil
			},
		},
	}
}
