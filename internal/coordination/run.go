package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	ErrNilStore             = errors.New("coordination: nil store")
	errTaskReportProjection = errors.New("coordination: task report projection failed")
)

// Run 由图调度一次从 taskID 出发的执行，返回该 task 的 verifier 输出。
//
// 每个角色节点顺序是 join → Ask → spawn：
//   - join：Ask 前等 IncomingJoins 的子 task 结束，Merge 子环境，把子输出拼进本节点输入。
//   - Ask：跑这个角色的 ReAct；ProgressStore 已有输出则跳过。
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
	return g.run(ctx, taskID, input, stores, assemble, nil)
}

// RunWithReport 在写入任务终态前提交根 task 报告；报告失败时保留 active 状态和进度供重试。
func (g *Graph) RunWithReport(
	ctx context.Context,
	taskID string,
	input string,
	stores Stores,
	assemble AssembleFunc,
	report func(Task, string, error) error,
) (string, error) {
	return g.run(ctx, taskID, input, stores, assemble, report)
}

func (g *Graph) run(
	ctx context.Context,
	taskID string,
	input string,
	stores Stores,
	assemble AssembleFunc,
	report func(Task, string, error) error,
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
	help := g.help
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.executing = false
		g.mu.Unlock()
	}()
	r := &runner{
		graph:      g,
		stores:     stores,
		assemble:   assemble,
		progress:   progress,
		cancel:     cancel,
		childDone:  make(map[string]chan taskResult),
		nodeDone:   make(map[string]chan struct{}),
		nodeOutput: make(map[string]string),
		started:    map[string]struct{}{taskID: {}},
	}
	if help != nil {
		help.bind(r)
		defer help.bind(nil)
	}
	out, err := r.runTask(ctx, taskID, input)
	if err != nil {
		r.fail(err)
	}
	r.wg.Wait()
	if r.err != nil && !errors.Is(err, r.err) {
		err = errors.Join(err, r.err)
	}
	if report != nil {
		task, ok := g.Task(taskID)
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
		}
		task.Outcome = outcomeForError(err)
		if reportErr := report(task, out, err); reportErr != nil {
			return "", errors.Join(err, reportErr)
		}
	}
	if errors.Is(err, errTaskReportProjection) {
		return "", err
	}
	if persistErr := g.recordOutcome(taskID, err); persistErr != nil {
		return "", errors.Join(err, persistErr)
	}
	if err != nil {
		return "", err
	}
	if err := r.discardTree(taskID); err != nil {
		return "", err
	}
	return out, nil
}

func outcomeForError(err error) string {
	switch {
	case err == nil:
		return OutcomeDone
	case canceledOnly(err):
		return OutcomeCanceled
	default:
		return OutcomeFailed
	}
}

func canceledOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return errors.Is(err, context.Canceled)
		}
		for _, child := range children {
			if !canceledOnly(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		return canceledOnly(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}

type taskResult struct {
	output string
	err    error
}

// runner 是单次 Run 的调度状态，不写回 Graph。
type runner struct {
	graph      *Graph
	stores     Stores
	assemble   AssembleFunc
	progress   ProgressStore
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
	err        error
	childDone  map[string]chan taskResult
	nodeDone   map[string]chan struct{}
	nodeOutput map[string]string
	started    map[string]struct{}
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

	parentID := task.Env.ParentID
	if parentID == "" {
		parentID = ManagerEnvID
	}
	if err := r.stores.Fork(parentID, task.Env.ID); err != nil {
		return "", err
	}
	if task.Env.ParentID == "" {
		if err := r.stores.Memory.DropSubgraph(task.Env.ID, ManagerMemorySubgraphID); err != nil {
			return "", err
		}
	}
	outputs, merged, prepared, err := r.loadProgress(taskID)
	if err != nil {
		return "", err
	}
	for id, saved := range outputs {
		r.markNodeOutput(id, saved)
	}

	roles, err := r.assemble(task)
	if err != nil {
		return "", err
	}
	if !prepared && roles.Prepare != nil {
		if err := roles.Prepare(ctx); err != nil {
			return "", err
		}
		if err := r.saveProgress(taskID, outputs, merged, true); err != nil {
			return "", err
		}
	}

	output = input
	sequence := task.Sequence()
	for i, node := range sequence {
		if err := ctx.Err(); err != nil {
			return "", errors.Join(err, r.drainJoins(ctx, task, sequence[i:], outputs, merged))
		}
		output, err = r.runRole(ctx, node, roles, output, outputs, merged)
		if err != nil {
			r.fail(err)
			return "", errors.Join(err, r.drainJoins(ctx, task, sequence[i+1:], outputs, merged))
		}
	}
	return output, nil
}

func (r *runner) drainJoins(ctx context.Context, task Task, nodes []Node, outputs map[string]string, merged map[string]bool) error {
	var joinedErr error
	for _, node := range nodes {
		_, err := r.joinIncoming(ctx, node, task, "", outputs, merged)
		joinedErr = errors.Join(joinedErr, err)
	}
	return joinedErr
}

// runRole 执行图上的一个角色节点：join → Ask → spawn。
func (r *runner) runRole(ctx context.Context, node Node, roles Roles, input string, outputs map[string]string, merged map[string]bool) (string, error) {
	asker := roles.asker(node.Role)
	if asker == nil {
		return "", fmt.Errorf("%w: %s", ErrNilAsker, node.Role)
	}
	task, ok := r.graph.Task(node.TaskID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTask, node.TaskID)
	}

	input, err := r.joinIncoming(ctx, node, task, input, outputs, merged)
	if err != nil {
		return "", err
	}

	output, ok := outputs[node.ID]
	if !ok {
		output, err = asker.Ask(ctx, input)
		if err != nil {
			return "", err
		}
		outputs[node.ID] = output
		if err := r.saveProgress(node.TaskID, outputs, merged, true); err != nil {
			return "", err
		}
	}
	r.markNodeOutput(node.ID, output)

	spawned := r.graph.SpawnedTasks(node.ID)
	for _, child := range spawned {
		childInput := spawnInput(child.Info, output)
		if err := r.startChild(ctx, child, childInput); err != nil {
			return "", err
		}
	}
	return output, nil
}

func (r *runner) runHelp(ctx context.Context, task Task, requestID string, children []helpChild) (string, error) {
	if result, ok, err := r.restoredHelpResult(task.ID, requestID, children); err != nil {
		return "", err
	} else if ok {
		return result, nil
	}
	for _, child := range children {
		upstream, err := r.waitNodeOutput(ctx, child.from)
		if err != nil {
			return "", err
		}
		if err := r.startChild(ctx, child.task, spawnInput(child.task.Info, upstream)); err != nil {
			return "", err
		}
	}
	tasks := make([]Task, 0, len(children))
	for _, child := range children {
		tasks = append(tasks, child.task)
	}
	joined, err := r.joinTasks(ctx, task, tasks, false)
	if err != nil {
		return "", err
	}
	if err := r.markHelpMerged(task.ID, requestID); err != nil {
		return "", err
	}
	return joinedTaskOutput(joined), nil
}

func (r *runner) restoredHelpResult(taskID, requestID string, children []helpChild) (string, bool, error) {
	if r.progress == nil {
		return "", false, nil
	}
	progress, ok, err := r.progress.Load(taskID)
	if err != nil {
		return "", false, fmt.Errorf("loading task progress: %w", err)
	}
	if !ok || !hasProgressID(progress.Merged, helpProgressID(requestID)) {
		return "", false, nil
	}
	joined := make([]joinedTask, 0, len(children))
	for _, child := range children {
		progress, ok, err := r.progress.Load(child.task.ID)
		if err != nil {
			return "", false, fmt.Errorf("loading help task progress: %w", err)
		}
		if !ok {
			return "", false, fmt.Errorf("loading help task progress: %s is missing", child.task.ID)
		}
		joined = append(joined, joinedTask{
			task: child.task,
			out:  progress.Outputs[child.task.Verifier.ID],
		})
	}
	return joinedTaskOutput(joined), true, nil
}

func (r *runner) markHelpMerged(taskID, requestID string) error {
	if r.progress == nil {
		return nil
	}
	progress, _, err := r.progress.Load(taskID)
	if err != nil {
		return fmt.Errorf("loading task progress: %w", err)
	}
	marker := helpProgressID(requestID)
	if hasProgressID(progress.Merged, marker) {
		return nil
	}
	progress.Merged = append(progress.Merged, marker)
	if err := r.progress.Save(taskID, progress); err != nil {
		return fmt.Errorf("saving task progress: %w", err)
	}
	return nil
}

func helpProgressID(requestID string) string {
	return "help:" + requestID
}

func (r *runner) startChild(ctx context.Context, child Task, input string) error {
	if err := r.stores.Fork(child.Env.ParentID, child.Env.ID); err != nil {
		return err
	}
	r.mu.Lock()
	if _, ok := r.started[child.ID]; ok {
		r.mu.Unlock()
		return nil
	}
	r.started[child.ID] = struct{}{}
	r.mu.Unlock()

	done := r.childCh(child.ID)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		out, err := r.runTask(ctx, child.ID, input)
		done <- taskResult{output: out, err: err}
		if err != nil {
			r.fail(err)
		}
	}()
	return nil
}

func (r *runner) markNodeOutput(nodeID, output string) {
	r.mu.Lock()
	if _, exists := r.nodeOutput[nodeID]; exists {
		r.mu.Unlock()
		return
	}
	r.nodeOutput[nodeID] = output
	done := r.nodeDone[nodeID]
	if done != nil {
		close(done)
	}
	r.mu.Unlock()
}

func (r *runner) waitNodeOutput(ctx context.Context, nodeID string) (string, error) {
	r.mu.Lock()
	if output, ok := r.nodeOutput[nodeID]; ok {
		r.mu.Unlock()
		return output, nil
	}
	done := r.nodeDone[nodeID]
	if done == nil {
		done = make(chan struct{})
		r.nodeDone[nodeID] = done
	}
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-done:
		r.mu.Lock()
		output := r.nodeOutput[nodeID]
		r.mu.Unlock()
		return output, nil
	}
}

func (r *runner) joinIncoming(ctx context.Context, node Node, task Task, input string, outputs map[string]string, merged map[string]bool) (string, error) {
	preds := r.graph.IncomingJoins(node.ID)
	if len(preds) == 0 {
		return input, nil
	}
	children := make([]Task, 0, len(preds))
	for _, pred := range preds {
		if r.graph.isHelpChildJoin(node.ID, pred.TaskID) {
			continue
		}
		child, ok := r.graph.Task(pred.TaskID)
		if ok {
			children = append(children, child)
		}
	}
	if len(children) == 0 {
		return input, nil
	}
	already := merged[node.ID]
	joined, err := r.joinTasks(ctx, task, children, already)
	if err != nil {
		return "", err
	}
	if !already {
		merged[node.ID] = true
		if err := r.saveProgress(node.TaskID, outputs, merged, true); err != nil {
			return "", err
		}
	}
	return input + "\n\n" + joinedTaskOutput(joined), nil
}

type joinedTask struct {
	task Task
	out  string
}

func (r *runner) joinTasks(ctx context.Context, parent Task, children []Task, already bool) ([]joinedTask, error) {
	joined := make([]joinedTask, 0, len(children))
	var joinedErr error
	for _, child := range children {
		out := r.savedTaskOutput(child.ID)
		if !already {
			var err error
			out, err = r.waitTask(ctx, child.ID)
			if err != nil {
				reportErr := r.projectJoinedTaskReport(parent, child, fmt.Sprintf("任务未完成：%v", err))
				joinedErr = errors.Join(joinedErr, err, reportErr)
				continue
			}
			if err := r.stores.Merge(child.Env.ID, parent.Env.ID); err != nil {
				joinedErr = errors.Join(joinedErr, err)
			}
		}
		if err := r.projectJoinedTaskReport(parent, child, out); err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
		joined = append(joined, joinedTask{task: child, out: out})
	}
	return joined, joinedErr
}

func (r *runner) projectJoinedTaskReport(parent, child Task, output string) error {
	if err := r.stores.ProjectJoinedTaskReport(parent, child, output); err != nil {
		return fmt.Errorf("%w for task %s: %w", errTaskReportProjection, child.ID, err)
	}
	return nil
}

func joinedTaskOutput(joined []joinedTask) string {
	var output strings.Builder
	for _, item := range joined {
		fmt.Fprintf(&output, "[join] 子任务 %s 输出：\n%s\n", item.task.ID, item.out)
	}
	return strings.TrimSpace(output.String())
}

func (g *Graph) recordOutcome(rootID string, err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	tree := g.spawnedSubtreeLocked(rootID)
	for id := range tree {
		g.setOutcomeLocked(id, outcomeForError(err))
	}
	helps := g.helps[:0]
	for _, help := range g.helps {
		if node, ok := g.nodeByIDLocked(help.NodeID); ok {
			if _, remove := tree[node.TaskID]; remove {
				continue
			}
		}
		helps = append(helps, help)
	}
	g.helps = helps
	return g.saveOrRestoreLocked(before)
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
	done := r.childCh(taskID)
	select {
	case res := <-done:
		if res.err != nil {
			return "", res.err
		}
		return res.output, nil
	case <-ctx.Done():
		r.mu.Lock()
		_, started := r.started[taskID]
		r.mu.Unlock()
		if started {
			res := <-done
			if res.err != nil {
				return "", res.err
			}
			return res.output, nil
		}
		select {
		case res := <-done:
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

func (r *runner) loadProgress(taskID string) (map[string]string, map[string]bool, bool, error) {
	outputs := make(map[string]string)
	merged := make(map[string]bool)
	if r.progress == nil {
		return outputs, merged, false, nil
	}
	progress, ok, err := r.progress.Load(taskID)
	if err != nil || !ok {
		return outputs, merged, false, err
	}
	for id, output := range progress.Outputs {
		outputs[id] = output
	}
	for _, id := range progress.Merged {
		merged[id] = true
	}
	return outputs, merged, progress.Prepared, nil
}

func (r *runner) saveProgress(
	taskID string,
	outputs map[string]string,
	merged map[string]bool,
	prepared bool,
) error {
	if r.progress == nil {
		return nil
	}
	current, ok, err := r.progress.Load(taskID)
	if err != nil {
		return fmt.Errorf("loading task progress: %w", err)
	}
	if ok {
		for _, id := range current.Merged {
			merged[id] = true
		}
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
	if err := r.progress.Save(taskID, TaskProgress{
		Outputs: copied, Merged: mergedIDs, Prepared: prepared,
	}); err != nil {
		return fmt.Errorf("saving task progress: %w", err)
	}
	return nil
}

func hasProgressID(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
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
