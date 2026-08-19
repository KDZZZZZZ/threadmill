package coordination

import (
	"context"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const managerEnvID = "manager"

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
	stores Stores,
	provider agent.Provider,
	agents agent.FileAgents,
	extra []agenttool.Tool,
	contextWindow int,
	checkpoints agent.CheckpointStore,
	overlay ...agent.FileOverlay,
) AssembleFunc {
	return func(task Task) (Roles, error) {
		e, err := openEnv(stores, task.Env.ID)
		if err != nil {
			return Roles{}, err
		}
		team, err := agent.NewTeam(
			provider,
			contextWindow,
			agents,
			extra,
			overlay...,
		)
		if err != nil {
			return Roles{}, err
		}
		team.BindCheckpoints(checkpoints, task.ID)
		if err := team.Bind(e); err != nil {
			return Roles{}, err
		}
		return Roles{
			Planner:  team.Planner,
			Executor: team.Executor,
			Verifier: team.Verifier,
		}, nil
	}
}

// NewManagerLoop 装配长命的经理 Agent，装上本图的协调图工具，并绑到独立的 manager 环境。
func NewManagerLoop(
	graph *Graph,
	stores Stores,
	provider agent.Provider,
	agents agent.FileAgents,
	extra []agenttool.Tool,
	contextWindow int,
	overlay agent.FileOverlay,
) (*agent.Loop, error) {
	overlay.NamedTools = GraphToolMap(graph)
	loop, err := agent.NewManager(provider, contextWindow, agents, extra, overlay)
	if err != nil {
		return nil, err
	}
	if err := loop.AddHooks(InjectCoordinationGraph(graph)); err != nil {
		return nil, err
	}
	e, err := openEnv(stores, managerEnvID)
	if err != nil {
		return nil, err
	}
	if err := loop.Bind(e); err != nil {
		return nil, err
	}
	return loop, nil
}

func openEnv(stores Stores, envID string) (env.Env, error) {
	if stores.Memory == nil {
		return env.Env{}, ErrNilStore
	}
	e := env.Open(envID, stores.Memory.View(envID))
	if stores.Files != nil {
		e = e.WithFiles(filesView{view: stores.Files.View(envID)})
	}
	if stores.Exec != nil && stores.Files != nil {
		e = e.WithExec(stores.Exec.View(envID, stores.Files))
	}
	return e, nil
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

var _ env.FileView = filesView{}

// filesView 把 vfs.View 的本地 FileInfo/DirEnt 转成 env.FileView。
type filesView struct {
	view *vfs.View
}

func (v filesView) Read(path string) ([]byte, error) {
	return v.view.Read(path)
}

func (v filesView) Write(path string, data []byte) error {
	return v.view.Write(path, data)
}

func (v filesView) Delete(path string) error {
	return v.view.Delete(path)
}

func (v filesView) Stat(path string) (env.FileInfo, error) {
	info, err := v.view.Stat(path)
	if err != nil {
		return env.FileInfo{}, err
	}
	return env.FileInfo{Name: info.Name, Size: info.Size, IsDir: info.IsDir}, nil
}

func (v filesView) List(path string) ([]env.DirEnt, error) {
	ents, err := v.view.List(path)
	if err != nil {
		return nil, err
	}
	out := make([]env.DirEnt, len(ents))
	for i, ent := range ents {
		out[i] = env.DirEnt{Name: ent.Name, IsDir: ent.IsDir}
	}
	return out, nil
}
