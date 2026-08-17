package coordination

import (
	"context"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

// Asker 执行一个角色的 ReAct 循环。
type Asker interface {
	Ask(ctx context.Context, query string) (string, error)
}

// Roles 是一个 task 装配出的 planner、executor、verifier。
type Roles struct {
	Planner  Asker
	Executor Asker
	Verifier Asker
}

// AssembleFunc 按 task 组装三个角色；工具必须绑到 task.Env。
type AssembleFunc func(Task) (Roles, error)

// Assemble 按 yaml 装配 prompt、tool、hook，再把记忆图绑到 task.Env。
// contextWindow 来自 llm.context_window。checkpoints 保存进行中的 ReAct，可为空。
func Assemble(
	store *ctxgraph.Store,
	provider agent.Provider,
	agents agent.FileAgents,
	extra []agenttool.Tool,
	contextWindow int,
	checkpoints agent.CheckpointStore,
) AssembleFunc {
	return func(task Task) (Roles, error) {
		if store == nil {
			return Roles{}, ErrNilStore
		}
		team, err := agent.NewTeam(
			provider,
			contextWindow,
			agents,
			extra,
		)
		if err != nil {
			return Roles{}, err
		}
		team.BindCheckpoints(checkpoints, task.ID)
		if err := team.Bind(store, task.Env.ID); err != nil {
			return Roles{}, err
		}
		return Roles{
			Planner:  team.Planner,
			Executor: team.Executor,
			Verifier: team.Verifier,
		}, nil
	}
}

func (r Roles) asker(role string) Asker {
	switch role {
	case RolePlanner:
		return r.Planner
	case RoleExecutor:
		return r.Executor
	case RoleVerifier:
		return r.Verifier
	default:
		return nil
	}
}
