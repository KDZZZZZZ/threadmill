package coordination

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const (
	ManagerEnvID            = "manager"
	ManagerMemorySubgraphID = "system-manager"
	taskSourcesSubgraphID   = "system-task-sources"
)

// ManagerMemorySubgraph 是 manager 独占的用户消息、task info 和报告视图。
func ManagerMemorySubgraph() ctxgraph.Subgraph {
	return ctxgraph.Subgraph{
		ID:      ManagerMemorySubgraphID,
		Name:    "manager context",
		Summary: "用户消息、task info 与 task 报告",
		Kind:    ctxgraph.SubgraphKindSystem,
	}
}

// TaskPackageSubgraph 是 task 的初始记忆与输入整理结果组成的固定启动包。
func TaskPackageSubgraph(taskID string) ctxgraph.Subgraph {
	return ctxgraph.Subgraph{
		ID:      taskID + "-package",
		Name:    "task startup package",
		Summary: "执行任务所需的最小初始记忆与输入整理结果",
		Kind:    ctxgraph.SubgraphKindPackage,
	}
}

func taskSourcesSubgraph() ctxgraph.Subgraph {
	return ctxgraph.Subgraph{
		ID:      taskSourcesSubgraphID,
		Name:    "task source requests",
		Summary: "task 创建时对应的原始用户请求",
		Kind:    ctxgraph.SubgraphKindSystem,
	}
}

// Asker 执行一个角色的 ReAct 循环。
type Asker interface {
	Ask(ctx context.Context, query string) (string, error)
}

// Roles 是一个 task 装配出的 planner、executor、verifier。
type Roles struct {
	Planner        Asker
	Executor       Asker
	Verifier       Asker
	ResolveInput   func(context.Context, Node, InputProgress) error
	OrganizeMemory func(context.Context, agent.InputMemoryRequest) (ctxgraph.Graph, error)
	bind           func(role, memoryID, workspaceID string) error
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
	var events *event.Bus
	if len(overlay) > 0 {
		events = overlay[0].Events
	}
	return func(task Task) (Roles, error) {
		if stores.Memory == nil {
			return Roles{}, ErrNilStore
		}
		taskAgents := agents
		if !task.RealDirectory {
			for _, role := range []*agent.FileAgent{&taskAgents.Planner, &taskAgents.Executor, &taskAgents.Verifier} {
				role.Tools = slices.DeleteFunc(slices.Clone(role.Tools), func(name string) bool {
					return name == coordMessageManagerName
				})
			}
		}
		team, err := agent.NewTeam(provider, contextWindow, taskAgents, extra, overlay...)
		if err != nil {
			return Roles{}, err
		}
		team.BindCheckpoints(checkpoints, strings.TrimSuffix(task.Planner.ID, ":"+RolePlanner))
		pack := TaskPackageSubgraph(task.ID)
		for _, loop := range []*agent.Loop{team.Planner, team.Executor, team.Verifier} {
			loop.SetStableSubscribedSubgraphs([]string{pack.ID})
		}
		bind := func(role, memoryID, workspaceID string) error {
			if err := prepareTaskPackage(stores, task, memoryID); err != nil {
				return err
			}
			e, err := openRoleEnv(stores, memoryID, workspaceID)
			if err != nil {
				return err
			}
			loop := roleLoop(team, role)
			if loop == nil {
				return fmt.Errorf("%w: %s", ErrNilAsker, role)
			}
			return loop.Bind(e)
		}
		// Direct Assemble callers retain a usable team. Run binds again only after
		// installing the ready pair, so assembly cannot inject candidate memory.
		for _, role := range []string{RolePlanner, RoleExecutor, RoleVerifier} {
			if err := bind(role, task.Env.ID, task.Env.ID); err != nil {
				return Roles{}, err
			}
		}
		organizer := agents.SubgraphOrganizer
		roles := Roles{
			Planner: team.Planner, Executor: team.Executor, Verifier: team.Verifier, bind: bind,
			OrganizeMemory: func(ctx context.Context, request agent.InputMemoryRequest) (ctxgraph.Graph, error) {
				return agent.OrganizeInputMemory(ctx, agent.Config{
					AgentID: task.Env.ID + ":input-organizer", Provider: provider,
					ContextWindow: contextWindow, MaxSteps: organizer.MaxSteps,
					SystemPrompt: organizer.SystemPrompt,
					Events:       events,
				}, request, overlay...)
			},
		}
		var inputTool agenttool.Tool
		for _, item := range extra {
			if item.Definition().Name == inputToolName {
				inputTool = item
			}
		}
		for _, item := range overlay {
			if item.NamedTools[inputToolName] != nil {
				inputTool = item.NamedTools[inputToolName]
			}
		}
		if inputTool != nil {
			roles.ResolveInput = func(ctx context.Context, node Node, input InputProgress) error {
				tools := append(agenttool.FileTools(), agenttool.Bash(), inputTool)
				if len(overlay) > 0 {
					tools = agent.WithToolDescriptions(tools, overlay[0].Tools)
				}
				loop, err := agent.NewLoop(agent.Config{
					AgentID: node.ID, Provider: provider, Tools: tools, ContextWindow: contextWindow,
					MaxSteps:     agents.Executor.MaxSteps,
					Events:       events,
					SystemPrompt: "只处理本次固定来源的文件差异。共同文件已经直接使用。用 input 查看和选择差异，必要时编辑、验证当前草稿；为未采纳的候选注明原因，完成后调用 input finish。不得执行原任务、整理记忆或把候选报告当作当前事实。文件处理完成后立即结束。",
				})
				if err != nil {
					return err
				}
				e, err := openRoleEnv(stores, input.ID+":scratch", input.TargetID)
				if err != nil {
					return err
				}
				if err := loop.Bind(e); err != nil {
					return err
				}
				_, err = askRole(ctx, loop, taskInput(task.Info, inputNotice(input)))
				return err
			}
		}
		return roles, nil
	}
}

func prepareTaskPackage(stores Stores, task Task, memoryID string) error {
	pack := TaskPackageSubgraph(task.ID)
	if err := stores.Memory.DropSubgraph(memoryID, ManagerMemorySubgraphID); err != nil {
		return err
	}
	if err := stores.Memory.DropSubgraph(memoryID, taskSourcesSubgraphID); err != nil {
		return err
	}
	if err := stores.Memory.EnsureSubgraph(memoryID, pack); err != nil {
		return err
	}
	for _, source := range stores.Memory.Load(ManagerEnvID).NodesInSubgraphs([]string{taskSourcesSubgraphID}) {
		if source.ID == taskUserInputNodeID(task.ID) {
			if err := stores.Memory.AppendNode(memoryID, pack, source); err != nil {
				return err
			}
			break
		}
	}
	if task.Info != "" {
		return stores.Memory.AppendNode(memoryID, pack, taskInfoNode(task))
	}
	return nil
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
	if stores.Memory == nil {
		return nil, ErrNilStore
	}
	if overlay.NamedTools == nil {
		overlay.NamedTools = make(map[string]agenttool.Tool)
	}
	for name, tool := range GraphToolMap(graph) {
		overlay.NamedTools[name] = tool
	}
	loop, err := agent.NewManager(provider, contextWindow, agents, extra, overlay)
	if err != nil {
		return nil, err
	}
	if err := loop.AddHooks(InjectCoordinationGraph(graph)); err != nil {
		return nil, err
	}
	memory := ManagerMemorySubgraph()
	if err := stores.Memory.EnsureSubgraph(ManagerEnvID, memory); err != nil {
		return nil, err
	}
	loop.SetFixedSubscribedSubgraphs([]string{memory.ID})
	e, err := openEnv(stores, ManagerEnvID)
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
