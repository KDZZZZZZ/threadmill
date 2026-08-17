package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// Run 由图调度一次从 taskID 出发的执行，返回该 task 的 verifier 在声明完成后的输出。
//
// 调度只认图上的边，规则是：
//   - 开始：同一 task 里前置阶段「声明完成」后才能开始下一阶段（planner → executor → verifier）。
//     某个角色一开始执行，从它 spawn 出去的子 task 也立刻开始（并行，不等子 task 结束）。
//   - 完成：一个节点当且仅当「所有指向自己的节点都已声明完成」并且「自己的 ReAct 已跑完」
//     之后，才能声明完成。指向自己的边包括 sequence、spawn、join。
//
// 对每个被调度到的 task，顺序固定为：
//  1. 装配环境：stores.Fork(父 env, 本 task.env)，子环境不存在时拷贝父快照，已存在则不覆盖。
//  2. 组装 agent：assemble(task) 得到三个角色；工具必须绑在 task.Env 上。
//  3. 执行 ReAct：对当前角色调用 Asker.Ask（即 agent.Loop 的 ReAct 循环）。
//     若设置了 ProgressStore，已完成角色跳过 Ask，用保存的输出继续；入口 Run 成功后扔掉整棵子树的进度。
//
// Join 挡住合入点的「声明完成」，不挡住它开始 Ask。合入点先跑 ReAct，等 Incoming 都完成后，
// 再把每条 join 边的子 task 环境 Merge 进合入点的 task 环境。
//
// spawn 边指向子 task 的 planner，所以子 planner 可以和父角色同时 Ask，但必须等父角色
// 声明完成后才能自己声明完成。Join 指回更早阶段、或 spawn 与 join 落在同一节点时，
// 完成依赖会成环，会一直等到 ctx 取消。
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

	// 子 goroutine 出错时 cancel，让其它节点在 waitDone 上被唤醒，避免死等完成事件。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g.mu.Lock()
	progress := g.progress
	g.mu.Unlock()
	r := &runner{
		graph:    g,
		stores:   stores,
		assemble: assemble,
		progress: progress,
		cancel:   cancel,
		done:     make(map[string]chan struct{}),
	}
	out, err := r.runTask(ctx, taskID, input)
	// spawn 出去的子 task 在独立 goroutine 里跑；入口 verifier 完成后它们通常已结束，
	// 这里再等一遍，避免 Run 返回后还有 goroutine 碰 stores。
	r.wg.Wait()
	if r.err != nil {
		return "", r.err
	}
	if err != nil {
		return "", err
	}
	if err := r.discardTree(taskID); err != nil {
		return "", err
	}
	return out, nil
}

// runner 是单次 Run 的调度状态，不写回 Graph。图只提供拓扑（Sequence / SpawnedTasks / Incoming）。
type runner struct {
	graph    *Graph
	stores   Stores
	assemble AssembleFunc
	progress ProgressStore
	cancel   context.CancelFunc
	wg       sync.WaitGroup           // 跟踪 spawn 出去的子 task goroutine
	mu       sync.Mutex               // 保护 err 与 done
	err      error                    // 本次 Run 的第一个错误
	done     map[string]chan struct{} // 节点 ID → 声明完成事件（close 表示完成）
}

// runTask 调度一个 task：先 fork 环境、再组装三个 agent，然后按 sequence 逐个 runRole。
// runRole 返回前该节点已声明完成，因此下一阶段开始时前置阶段一定已完成。
func (r *runner) runTask(ctx context.Context, taskID, input string) (output string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	task, ok := r.graph.Task(taskID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	defer func() {
		if r.stores.Files == nil {
			return
		}
		aerr := r.stores.Files.Absorb(task.Env.ID)
		rerr := r.stores.Files.Release(task.Env.ID)
		if err != nil {
			return
		}
		if aerr != nil {
			err = aerr
			return
		}
		err = rerr
	}()

	r.stores.Fork(task.Env.ParentID, task.Env.ID)
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

// runRole 执行图上的一个角色节点。
//
//  1. 一开始就把从本节点 spawn 出的子 task 拉起来（各走一遍 runTask），不等它们结束。
//     子 task 拿到的输入是本角色此刻的输入，因为父角色的 Ask 还没返回。
//  2. Ask：跑这个角色的 ReAct。
//  3. 等 Incoming 里每个节点的完成事件（sequence 前驱、spawn 来源、join 进来的 verifier）。
//  4. 对每条 join 入边，把前驱 task 环境 Merge 进本节点的 task 环境。
//  5. finish：发出自己的完成事件。同一 task 的下一阶段、以及所有等这条边的节点，现在可以往下走。
func (r *runner) runRole(ctx context.Context, node Node, roles Roles, input string, outputs map[string]string, merged map[string]bool) (string, error) {
	asker := roles.asker(node.Role)
	if asker == nil {
		return "", fmt.Errorf("%w: %s", ErrNilAsker, node.Role)
	}
	task, ok := r.graph.Task(node.TaskID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTask, node.TaskID)
	}

	for _, child := range r.graph.SpawnedTasks(node.ID) {
		childID := child.ID
		r.stores.Fork(task.Env.ID, child.Env.ID)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			if _, err := r.runTask(ctx, childID, input); err != nil {
				r.fail(err)
			}
		}()
	}

	output, ok := outputs[node.ID]
	if !ok {
		var err error
		output, err = asker.Ask(ctx, input)
		if err != nil {
			return "", err
		}
		outputs[node.ID] = output
		if err := r.saveProgress(node.TaskID, outputs, merged); err != nil {
			return "", err
		}
	}
	for _, pred := range r.graph.Incoming(node.ID) {
		if err := r.waitDone(ctx, pred.ID); err != nil {
			return "", err
		}
	}
	target := task
	if !merged[node.ID] {
		for _, pred := range r.graph.IncomingJoins(node.ID) {
			child, ok := r.graph.Task(pred.TaskID)
			if !ok {
				continue
			}
			if err := r.stores.Merge(child.Env.ID, target.Env.ID); err != nil {
				return "", err
			}
		}
		merged[node.ID] = true
		if err := r.saveProgress(node.TaskID, outputs, merged); err != nil {
			return "", err
		}
	}
	r.finish(node.ID)
	return output, nil
}

// doneCh 返回节点的完成事件通道；首次访问时创建，这样 wait 可以发生在 finish 之前。
func (r *runner) doneCh(id string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.done[id]
	if !ok {
		ch = make(chan struct{})
		r.done[id] = ch
	}
	return ch
}

// finish 声明节点完成。重复调用是安全的（通道只 close 一次）。
func (r *runner) finish(id string) {
	ch := r.doneCh(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// waitDone 阻塞到指定节点声明完成，或本次 Run 的 ctx 被取消。
func (r *runner) waitDone(ctx context.Context, id string) error {
	select {
	case <-r.doneCh(id):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fail 记录本次 Run 的第一个错误并取消 ctx，让所有 waitDone 返回。
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
