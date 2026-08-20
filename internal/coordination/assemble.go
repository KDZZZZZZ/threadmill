package coordination

import (
	"context"
	"errors"
	"fmt"

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
	scope    func(string) (roleScope, error)
}

type roleScope struct {
	joinFilesID string
	prepare     func() (func(bool) error, error)
}

// AssembleFunc 按 task 组装三个角色。
type AssembleFunc func(Task) (Roles, error)

// Assemble 按 yaml 装配 prompt、tool、hook。executor 使用 task.Env；planner/verifier
// 使用一次性文件与执行分支，三者仍共享 task 记忆。
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
		e, err := openEnv(stores, task.Env.ID)
		if err != nil {
			return Roles{}, err
		}
		if err := team.Bind(e); err != nil {
			return Roles{}, err
		}
		return Roles{
			Planner:  team.Planner,
			Executor: team.Executor,
			Verifier: team.Verifier,
			scope: func(role string) (roleScope, error) {
				workspaceID := task.Env.ID
				joinFilesID := task.Env.ID
				disposable := false
				if stores.Files != nil {
					switch role {
					case RolePlanner, RoleVerifier:
						workspaceID = task.Env.ID + ":" + role
						joinFilesID = workspaceID
						disposable = true
					case RoleExecutor:
					default:
						return roleScope{}, fmt.Errorf("%w: %s", ErrNilAsker, role)
					}
				}
				return roleScope{
					joinFilesID: joinFilesID,
					prepare: func() (func(bool) error, error) {
						if disposable {
							if err := stores.Files.Fork(task.Env.ID, workspaceID); err != nil {
								return nil, err
							}
						}
						e, err := openRoleEnv(stores, task.Env.ID, workspaceID)
						if err != nil {
							return nil, err
						}
						loop := roleLoop(team, role)
						if loop == nil {
							return nil, fmt.Errorf("%w: %s", ErrNilAsker, role)
						}
						if err := loop.Bind(e); err != nil {
							if disposable {
								err = errors.Join(err, stores.DiscardFiles(workspaceID))
							}
							return nil, err
						}
						return func(completed bool) error {
							if !disposable {
								return nil
							}
							if completed {
								return stores.DiscardFiles(workspaceID)
							}
							if stores.Exec != nil {
								stores.Exec.Reap(workspaceID)
							}
							return stores.Files.Release(workspaceID)
						}, nil
					},
				}, nil
			},
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
	return openRoleEnv(stores, envID, envID)
}

func openRoleEnv(stores Stores, memoryID, workspaceID string) (env.Env, error) {
	if stores.Memory == nil {
		return env.Env{}, ErrNilStore
	}
	e := env.Open(workspaceID, stores.Memory.View(memoryID))
	if stores.Files != nil {
		e = e.WithFiles(filesView{view: stores.Files.View(workspaceID)})
	}
	if stores.Exec != nil && stores.Files != nil {
		e = e.WithExec(stores.Exec.View(workspaceID, stores.Files))
	}
	return e, nil
}

func roleLoop(team *agent.Team, role string) *agent.Loop {
	switch role {
	case RolePlanner:
		return team.Planner
	case RoleExecutor:
		return team.Executor
	case RoleVerifier:
		return team.Verifier
	default:
		return nil
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
