package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

var (
	// ErrUnknownTask 表示要执行的 task 不在图中。
	ErrUnknownTask = errors.New("coordination: unknown task")
	// ErrNilAssemble 表示没有提供组装 agent 的函数。
	ErrNilAssemble = errors.New("coordination: nil assemble")
	// ErrNilAsker 表示某个角色没有可调用的 Asker。
	ErrNilAsker = errors.New("coordination: nil asker")
	// ErrNilStore 表示缺少按环境隔离的记忆存储。
	ErrNilStore = errors.New("coordination: nil store")
)

// Run 由图调度一次从 taskID 出发的执行，返回该 task 的 verifier 输出。
//
// 每个角色节点顺序是 fork → join → Ask → spawn：
//   - fork：目标角色先准备自己的文件与执行环境。
//   - join：Ask 前等 IncomingJoins 的子 task 结束，把子输出拼进本节点输入；
//     记忆合入 task 环境，文件放进目标角色的临时合入环境。
//   - Ask：目标角色在临时环境筛选文件、解决冲突并跑 ReAct；成功后提交选择。
//     ProgressStore 已有输出则跳过。
//   - spawn：Ask 之后 Fork 子环境，用本角色输出当子输入，拉起即走，不等待。
//
// 同一 task 的 planner → executor → verifier 由 runTask 的 for 循环保证。
// 入口 Run 成功后扔掉整棵子树的进度。
func (g *Graph) Run(
	ctx context.Context,
	taskID string,
	input string,
	stores Stores,
	assemble AssembleFunc,
) (string, error) {
	if ctx == nil {
		panic("nil context")
	}
	if assemble == nil {
		return "", ErrNilAssemble
	}
	if stores.Memory == nil {
		return "", ErrNilStore
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.mu.Lock()
	if g.executing {
		g.mu.Unlock()
		return "", ErrGraphBusy
	}
	g.executing = true
	progress := g.progress
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.executing = false
		g.mu.Unlock()
	}()
	r := &runner{
		graph:     g,
		stores:    stores,
		assemble:  assemble,
		progress:  progress,
		cancel:    cancel,
		childDone: make(map[string]chan taskResult),
	}
	out, err := r.runTask(ctx, taskID, input)
	r.wg.Wait()
	if r.err != nil {
		err = r.err
	}
	g.recordOutcome(taskID, err)
	if err != nil {
		return "", err
	}
	if err := r.discardTree(taskID); err != nil {
		return "", err
	}
	return out, nil
}

type taskResult struct {
	output string
	err    error
}

// runner 是单次 Run 的调度状态，不写回 Graph。
type runner struct {
	graph     *Graph
	stores    Stores
	assemble  AssembleFunc
	progress  ProgressStore
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.Mutex
	err       error
	childDone map[string]chan taskResult
}

// runTask 调度一个 task：先 fork 环境、再组装三个 agent，然后按 sequence 逐个 runRole。
func (r *runner) runTask(ctx context.Context, taskID, input string) (output string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	task, ok := r.graph.Task(taskID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	defer func() {
		if r.stores.Exec != nil {
			r.stores.Exec.Reap(task.Env.ID)
		}
		if r.stores.Files == nil {
			return
		}
		if rerr := r.stores.Files.Release(task.Env.ID); err == nil {
			err = rerr
		}
	}()

	if err := r.stores.Fork(task.Env.ParentID, task.Env.ID); err != nil {
		return "", err
	}
	roles, err := r.assemble(task)
	if err != nil {
		return "", err
	}

	outputs, merged, err := r.loadProgress(taskID)
	if err != nil {
		return "", err
	}

	output = input
	for _, node := range task.Sequence() {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		output, err = r.runRole(ctx, node, roles, output, outputs, merged)
		if err != nil {
			r.fail(err)
			return "", err
		}
	}
	return output, nil
}

// runRole 执行图上的一个角色节点：fork → join → Ask → spawn。
func (r *runner) runRole(ctx context.Context, node Node, roles Roles, input string, outputs map[string]string, merged map[string]bool) (string, error) {
	asker := roles.asker(node.Role)
	if asker == nil {
		return "", fmt.Errorf("%w: %s", ErrNilAsker, node.Role)
	}
	task, ok := r.graph.Task(node.TaskID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTask, node.TaskID)
	}

	output, completed := outputs[node.ID]
	scope := roleScope{workspaceID: task.Env.ID}
	var err error
	if roles.scope != nil {
		scope, err = roles.scope(node.Role)
		if err != nil {
			return "", err
		}
	}
	preparedJoin := roles.scope != nil && r.stores.Files != nil && len(r.graph.IncomingJoins(node.ID)) > 0
	activeID := scope.workspaceID
	if preparedJoin {
		activeID += ":join"
	}

	if !completed && scope.bind != nil {
		if err := scope.bind(activeID); err != nil {
			return "", err
		}
	}

	input, joined, err := r.joinIncoming(ctx, joinRequest{
		node:     node,
		task:     task,
		activeID: activeID,
		targetID: scope.workspaceID,
		prepared: preparedJoin,
		input:    input,
		outputs:  outputs,
		merged:   merged,
	})
	if err != nil {
		if scope.cleanup != nil {
			err = errors.Join(err, scope.cleanup(activeID, false))
		}
		return "", err
	}

	if !completed {
		output, err = asker.Ask(ctx, input)
		if err != nil {
			if scope.cleanup != nil {
				err = errors.Join(err, scope.cleanup(activeID, false))
			}
			return "", err
		}
		if joined.prepared {
			if err := r.stores.Files.CommitMerge(activeID, scope.workspaceID); err != nil {
				err = fmt.Errorf("committing joined files: %w", err)
				if scope.cleanup != nil {
					err = errors.Join(err, scope.cleanup(activeID, false))
				}
				return "", err
			}
			merged[node.ID] = true
		}
		outputs[node.ID] = output
		if err := r.saveProgress(node.TaskID, outputs, merged); err != nil {
			if scope.cleanup != nil {
				err = errors.Join(err, scope.cleanup(activeID, false))
			}
			return "", err
		}
	}
	if err := r.cleanupJoined(joined); err != nil {
		return "", err
	}
	if scope.cleanup != nil {
		if err := scope.cleanup(activeID, true); err != nil {
			return "", err
		}
	}

	spawned := r.graph.SpawnedTasks(node.ID)
	for _, child := range spawned {
		childID := child.ID
		if err := r.stores.Fork(task.Env.ID, child.Env.ID); err != nil {
			return "", err
		}
		done := r.childCh(childID)
		childInput := spawnInput(child.Info, output)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			out, err := r.runTask(ctx, childID, childInput)
			done <- taskResult{output: out, err: err}
			if err != nil {
				r.fail(err)
			}
		}()
	}
	return output, nil
}

type incomingJoin struct {
	child Task
	out   string
}

type joinedFiles struct {
	items    []incomingJoin
	prepared bool
}

type joinRequest struct {
	node     Node
	task     Task
	activeID string
	targetID string
	prepared bool
	input    string
	outputs  map[string]string
	merged   map[string]bool
}

func (r *runner) joinIncoming(ctx context.Context, req joinRequest) (string, joinedFiles, error) {
	preds := r.graph.IncomingJoins(req.node.ID)
	if len(preds) == 0 {
		return req.input, joinedFiles{}, nil
	}
	items := make([]incomingJoin, 0, len(preds))
	joined := joinedFiles{items: items, prepared: req.prepared}
	already := req.merged[req.node.ID]
	for _, pred := range preds {
		child, ok := r.graph.Task(pred.TaskID)
		if !ok {
			continue
		}
		var childOut string
		if already {
			childOut = r.savedTaskOutput(child.ID)
		} else {
			var err error
			childOut, err = r.waitTask(ctx, child.ID)
			if err != nil {
				return "", joined, err
			}
		}
		items = append(items, incomingJoin{child: child, out: childOut})
	}
	joined.items = items
	if !already {
		if req.prepared {
			sources := make([]vfs.MergeSource, 0, len(items))
			for _, item := range items {
				sources = append(sources, vfs.MergeSource{Name: item.child.ID, EnvID: item.child.Env.ID})
			}
			manifest, err := r.stores.Files.PrepareMerge(req.activeID, sources)
			if err != nil {
				return "", joined, fmt.Errorf("preparing joined files: %w", err)
			}
			for _, item := range items {
				if err := r.stores.Memory.Merge(item.child.Env.ID, req.task.Env.ID); err != nil {
					return "", joined, err
				}
			}
			review, err := mergeReviewInput(manifest)
			if err != nil {
				return "", joined, err
			}
			req.input += review
		} else {
			for _, item := range items {
				if err := r.stores.MergeInto(item.child.Env.ID, req.task.Env.ID, req.targetID); err != nil {
					return "", joined, err
				}
			}
			for _, item := range items {
				if err := r.stores.DiscardFiles(item.child.Env.ID); err != nil {
					return "", joined, err
				}
			}
			req.merged[req.node.ID] = true
			if err := r.saveProgress(req.node.TaskID, req.outputs, req.merged); err != nil {
				return "", joined, err
			}
		}
	}
	for _, item := range items {
		req.input += "\n\n[join] 子任务 " + item.child.ID + " 输出：\n" + item.out
	}
	return req.input, joined, nil
}

func (r *runner) cleanupJoined(joined joinedFiles) error {
	if !joined.prepared {
		return nil
	}
	var err error
	for _, item := range joined.items {
		err = errors.Join(err, r.stores.DiscardFiles(item.child.Env.ID))
	}
	return err
}

func mergeReviewInput(manifest vfs.MergeManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encoding joined file manifest: %w", err)
	}
	return "\n\n[join files] 子任务文件已放入临时合入工作区。无冲突改动已应用；冲突路径保留当前版本。" +
		"请用现有 read/write/edit/bash 工具检查 " + vfs.MergeRuntimeDir + "/manifest.json，" +
		"双方文件位于 ours/ 与 sources/。只把需要的内容写入正常项目路径；本轮结束时当前工作区即为最终选择。" +
		"\n合入清单：" + string(data), nil
}

func (g *Graph) recordOutcome(rootID string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case err == nil:
		for id := range g.spawnedSubtreeLocked(rootID) {
			g.setOutcomeLocked(id, OutcomeDone)
		}
	case errors.Is(err, context.Canceled):
		g.setOutcomeLocked(rootID, OutcomeCanceled)
	default:
		g.setOutcomeLocked(rootID, OutcomeFailed)
	}
}

func (g *Graph) setOutcomeLocked(id, outcome string) {
	for i := range g.tasks {
		if g.tasks[i].ID == id {
			g.tasks[i].Outcome = outcome
			return
		}
	}
}

func spawnInput(info, upstream string) string {
	switch {
	case info == "":
		return upstream
	case upstream == "":
		return info
	default:
		return info + "\n\n" + upstream
	}
}

func (r *runner) childCh(id string) chan taskResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.childDone[id]
	if !ok {
		ch = make(chan taskResult, 1)
		r.childDone[id] = ch
	}
	return ch
}

func (r *runner) waitTask(ctx context.Context, taskID string) (string, error) {
	select {
	case res := <-r.childCh(taskID):
		if res.err != nil {
			return "", res.err
		}
		return res.output, nil
	case <-ctx.Done():
		select {
		case res := <-r.childCh(taskID):
			if res.err != nil {
				return "", res.err
			}
			return res.output, nil
		default:
			return "", ctx.Err()
		}
	}
}

func (r *runner) savedTaskOutput(taskID string) string {
	task, ok := r.graph.Task(taskID)
	if !ok || r.progress == nil {
		return ""
	}
	progress, ok, err := r.progress.Load(taskID)
	if err != nil || !ok {
		return ""
	}
	return progress.Outputs[task.Verifier.ID]
}

func (r *runner) fail(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	r.err = err
	r.cancel()
}

func (r *runner) loadProgress(taskID string) (map[string]string, map[string]bool, error) {
	outputs := make(map[string]string)
	merged := make(map[string]bool)
	if r.progress == nil {
		return outputs, merged, nil
	}
	progress, ok, err := r.progress.Load(taskID)
	if err != nil || !ok {
		return outputs, merged, err
	}
	for id, output := range progress.Outputs {
		outputs[id] = output
	}
	for _, id := range progress.Merged {
		merged[id] = true
	}
	return outputs, merged, nil
}

func (r *runner) saveProgress(taskID string, outputs map[string]string, merged map[string]bool) error {
	if r.progress == nil {
		return nil
	}
	copied := make(map[string]string, len(outputs))
	for id, output := range outputs {
		copied[id] = output
	}
	mergedIDs := make([]string, 0, len(merged))
	for id, ok := range merged {
		if ok {
			mergedIDs = append(mergedIDs, id)
		}
	}
	if err := r.progress.Save(taskID, TaskProgress{Outputs: copied, Merged: mergedIDs}); err != nil {
		return fmt.Errorf("saving task progress: %w", err)
	}
	return nil
}

func (r *runner) discardTree(rootID string) error {
	if r.progress == nil {
		return nil
	}
	var err error
	for _, id := range r.graph.taskTree(rootID) {
		err = errors.Join(err, r.progress.Delete(id))
	}
	return err
}
