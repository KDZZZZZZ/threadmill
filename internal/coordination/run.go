package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
)

const maxAutomaticRoleRecoveries = 2

var (
	// ErrUnknownTask 表示要执行的 task 不在图中。
	ErrUnknownTask = errors.New("coordination: unknown task")
	// ErrNilAssemble 表示没有提供组装 agent 的函数。
	ErrNilAssemble = errors.New("coordination: nil assemble")
	// ErrNilAsker 表示某个角色没有可调用的 Asker。
	ErrNilAsker = errors.New("coordination: nil asker")
	// ErrNilStore 表示缺少按环境隔离的记忆存储。
	ErrNilStore = errors.New("coordination: nil store")
	// ErrRoleStalled 表示角色的持久回合在自动续接后仍遇到可恢复故障，需交给 manager 改变恢复策略。
	ErrRoleStalled          = errors.New("coordination: role stalled")
	errTaskReportProjection = errors.New("coordination: task report projection failed")
)

// Run 由图调度一次从 taskID 出发的执行，返回该 task 的 verifier 输出。
//
// 每个角色节点顺序是 fork → join → Ask → spawn：
//   - fork：目标角色先准备自己的文件与执行环境。
//   - join：Ask 前等 IncomingJoins 的子 task 结束，把候选注册给目标角色；
//     候选不会自动改文件，目标角色通过 join 工具检查并显式采纳或丢弃。
//   - Ask：目标角色处理全部 join session 后继续跑 ReAct。
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
	progress := g.progress
	help := g.help
	join := g.join
	r := &runner{
		graph:       g,
		stores:      stores,
		assemble:    assemble,
		progress:    progress,
		cancel:      cancel,
		childDone:   make(map[string]chan taskResult),
		nodeDone:    make(map[string]chan struct{}),
		nodeOutput:  make(map[string]string),
		started:     map[string]struct{}{taskID: {}},
		nodeStarted: make(map[string]struct{}),
		join:        join,
	}
	g.executing = true
	g.running = r
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.executing = false
		g.running = nil
		g.mu.Unlock()
	}()
	if help != nil {
		help.bind(r)
		defer help.bind(nil)
	}
	if join != nil {
		join.bind(r)
		defer join.bind(nil)
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

// runner 是单次 Run 的调度状态；Graph 仅在执行期间引用它来保护已开始切片。
type runner struct {
	graph       *Graph
	stores      Stores
	assemble    AssembleFunc
	progress    ProgressStore
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	mu          sync.Mutex
	err         error
	childDone   map[string]chan taskResult
	nodeDone    map[string]chan struct{}
	nodeOutput  map[string]string
	started     map[string]struct{}
	nodeStarted map[string]struct{}
	join        *joinCoordinator
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
			err = errors.Join(err, r.stores.Exec.Reap(task.Env.ID))
		}
		if r.stores.Files == nil {
			return
		}
		releaseErr := r.stores.Files.Release(task.Env.ID)
		archiveErr := r.stores.Files.Archive(task.Env.ID, taskSnapshotEnvID(task))
		err = errors.Join(err, releaseErr, archiveErr)
	}()

	parentID := task.Env.ParentID
	if parentID == "" {
		parentID = ManagerEnvID
	}
	if err := r.forkTaskEnvironment(task, parentID); err != nil {
		return "", err
	}
	if task.SpawnedFrom == "" && task.Env.ParentID != "" {
		if err := r.discardRootFiles(task.Env.ParentID); err != nil {
			return "", err
		}
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

func (r *runner) forkTaskEnvironment(task Task, parentID string) error {
	if task.SpawnedFrom != "" || task.Env.ParentID == "" || r.stores.Files == nil {
		return r.stores.Fork(parentID, task.Env.ID)
	}
	if err := r.stores.Memory.Fork(parentID, task.Env.ID); err != nil {
		return err
	}
	return r.stores.Files.Handoff(parentID, task.Env.ID)
}

func (r *runner) drainJoins(ctx context.Context, task Task, nodes []Node, outputs map[string]string, merged map[string]bool) error {
	var joinedErr error
	for _, node := range nodes {
		err := r.drainIncoming(ctx, node, task, outputs, merged)
		joinedErr = errors.Join(joinedErr, err)
	}
	return joinedErr
}

// runRole 执行图上的一个角色节点：fork → join → Ask → spawn。
func (r *runner) runRole(ctx context.Context, node Node, roles Roles, input string, outputs map[string]string, merged map[string]bool) (string, error) {
	r.markNodeStarted(node.ID)
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
	if !completed && scope.bind != nil {
		if err := scope.bind(); err != nil {
			return "", err
		}
	}

	input, joined, err := r.joinIncoming(ctx, joinRequest{
		node:     node,
		task:     task,
		targetID: scope.workspaceID,
		required: roles.scope != nil,
		input:    input,
		outputs:  outputs,
		merged:   merged,
	})
	if err != nil {
		if scope.cleanup != nil {
			err = errors.Join(err, scope.cleanup(false))
		}
		return "", err
	}

	if !completed {
		output, err = askRole(ctx, asker, input)
		if err != nil {
			if errors.Is(err, ErrRoleStalled) {
				err = fmt.Errorf("%s (%s): %w", node.ID, node.Role, err)
			}
			if scope.cleanup != nil {
				err = errors.Join(err, scope.cleanup(false))
			}
			return "", err
		}
		if r.join != nil {
			if err := r.join.requireFinished(node.ID); err != nil {
				if scope.cleanup != nil {
					err = errors.Join(err, scope.cleanup(false))
				}
				return "", err
			}
		}
		if len(joined.items) > 0 {
			merged[node.ID] = true
		}
		outputs[node.ID] = output
		if err := r.saveProgress(node.TaskID, outputs, merged, true); err != nil {
			if scope.cleanup != nil {
				err = errors.Join(err, scope.cleanup(false))
			}
			return "", err
		}
	}
	if scope.cleanup != nil {
		if err := scope.cleanup(true); err != nil {
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

func askRole(ctx context.Context, asker Asker, input string) (string, error) {
	for recoveries := 0; ; recoveries++ {
		output, err := asker.Ask(ctx, input)
		if err == nil || !agent.IsRecoverableTurnError(err) {
			return output, err
		}
		if recoveries >= maxAutomaticRoleRecoveries {
			return "", fmt.Errorf(
				"%w after %d automatic recoveries: %w",
				ErrRoleStalled,
				recoveries,
				err,
			)
		}
	}
}

type joinedFiles struct {
	items []joinedTask
}

type joinRequest struct {
	node     Node
	task     Task
	targetID string
	required bool
	input    string
	outputs  map[string]string
	merged   map[string]bool
}

func (r *runner) markNodeStarted(nodeID string) {
	r.mu.Lock()
	if r.nodeStarted == nil {
		r.nodeStarted = make(map[string]struct{})
	}
	r.nodeStarted[nodeID] = struct{}{}
	r.mu.Unlock()
}

func (r *runner) executionSnapshot() (map[string]struct{}, map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tasks := make(map[string]struct{}, len(r.started))
	for id := range r.started {
		tasks[id] = struct{}{}
	}
	nodes := make(map[string]struct{}, len(r.nodeStarted))
	for id := range r.nodeStarted {
		nodes[id] = struct{}{}
	}
	return tasks, nodes
}

func (r *runner) runHelp(ctx context.Context, task Task, nodeID, requestID string, children []helpChild) (string, error) {
	if result, ok, err := r.restoredHelpResult(task, nodeID, requestID, children); err != nil {
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
	joined, err := r.joinTaskReports(ctx, task, tasks, false)
	if err != nil {
		return "", err
	}
	if r.join == nil {
		return "", fmt.Errorf("coordination: join tool is unavailable")
	}
	session, err := r.join.open(
		task.ID,
		nodeID,
		roleWorkspaceID(task, nodeID),
		"join:help:"+requestID,
		joined,
	)
	if err != nil {
		return "", err
	}
	if err := r.markHelpJoined(task.ID, requestID); err != nil {
		return "", err
	}
	return joinNotice(session), nil
}

func (r *runner) restoredHelpResult(task Task, nodeID, requestID string, children []helpChild) (string, bool, error) {
	if r.progress == nil {
		return "", false, nil
	}
	progress, ok, err := r.progress.Load(task.ID)
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
	if r.join == nil {
		return "", false, fmt.Errorf("coordination: join tool is unavailable")
	}
	session, err := r.join.open(
		task.ID,
		nodeID,
		roleWorkspaceID(task, nodeID),
		"join:help:"+requestID,
		joined,
	)
	if err != nil {
		return "", false, err
	}
	return joinNotice(session), true, nil
}

func (r *runner) markHelpJoined(taskID, requestID string) error {
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
	r.mu.Lock()
	if _, ok := r.started[child.ID]; ok {
		r.mu.Unlock()
		return nil
	}
	r.started[child.ID] = struct{}{}
	r.mu.Unlock()
	if err := r.stores.Fork(child.Env.ParentID, child.Env.ID); err != nil {
		return err
	}

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

func (r *runner) joinIncoming(ctx context.Context, req joinRequest) (string, joinedFiles, error) {
	children := r.incomingChildren(req.node)
	if len(children) == 0 {
		return req.input, joinedFiles{}, nil
	}

	already := req.merged[req.node.ID]
	var (
		items []joinedTask
		err   error
	)
	items, err = r.joinTaskReports(ctx, req.task, children, already)
	joined := joinedFiles{items: items}
	if err != nil {
		return "", joined, err
	}

	if !already {
		if r.join == nil {
			if req.required {
				return "", joined, fmt.Errorf("coordination: join tool is unavailable")
			}
			// Minimal custom Askers used by graph-level callers have no tool
			// binding. They may observe scheduling, but never receive or apply
			// candidate artifacts.
			if err := r.discardJoinedFiles(items); err != nil {
				return "", joined, err
			}
			req.merged[req.node.ID] = true
			if err := r.saveProgress(req.node.TaskID, req.outputs, req.merged, true); err != nil {
				return "", joined, err
			}
			return req.input, joined, nil
		}
		session, err := r.join.open(
			req.node.TaskID,
			req.node.ID,
			req.targetID,
			"join:incoming:"+req.node.ID,
			items,
		)
		if err != nil {
			return "", joined, err
		}
		if !session.Finished {
			req.input += "\n\n" + joinNotice(session)
		}
	}
	return req.input, joined, nil
}

func (r *runner) incomingChildren(node Node) []Task {
	preds := r.graph.IncomingJoins(node.ID)
	children := make([]Task, 0, len(preds))
	for _, pred := range preds {
		if r.graph.isHelpChildJoin(node.ID, pred.TaskID) {
			continue
		}
		if child, ok := r.graph.Task(pred.TaskID); ok {
			children = append(children, child)
		}
	}
	return children
}

func (r *runner) drainIncoming(
	ctx context.Context,
	node Node,
	task Task,
	outputs map[string]string,
	merged map[string]bool,
) error {
	children := r.incomingChildren(node)
	if len(children) == 0 {
		return nil
	}
	already := merged[node.ID]
	joined, err := r.joinTaskReports(ctx, task, children, already)
	if err != nil {
		return err
	}
	if already {
		return nil
	}
	if err := r.discardJoinedFiles(joined); err != nil {
		return err
	}
	merged[node.ID] = true
	return r.saveProgress(node.TaskID, outputs, merged, true)
}

// joinTaskReports merges child memory and reports while leaving candidate files
// isolated for explicit role-level adoption through the join tool.
func (r *runner) joinTaskReports(ctx context.Context, parent Task, children []Task, already bool) ([]joinedTask, error) {
	joined := make([]joinedTask, 0, len(children))
	var joinedErr error
	for _, child := range children {
		out := r.savedTaskOutput(child.ID)
		if !already {
			var err error
			out, err = r.waitTask(ctx, child.ID)
			if err != nil {
				reportErr := r.projectCandidateTaskReport(parent, child, fmt.Sprintf("任务未完成：%v", err))
				joinedErr = errors.Join(joinedErr, err, reportErr)
				continue
			}
			if err := r.stores.Memory.Merge(child.Env.ID, parent.Env.ID); err != nil {
				joinedErr = errors.Join(joinedErr, err)
			}
		}
		if err := r.projectCandidateTaskReport(parent, child, out); err != nil {
			joinedErr = errors.Join(joinedErr, err)
		}
		joined = append(joined, joinedTask{task: child, out: out})
	}
	return joined, joinedErr
}

func (r *runner) discardJoinedFiles(joined []joinedTask) error {
	var err error
	for _, item := range joined {
		err = errors.Join(err, r.stores.DiscardFiles(item.task.Env.ID))
	}
	return err
}

func (r *runner) discardTaskFiles(envID string) error {
	var err error
	for _, suffix := range []string{
		"", ":" + RolePlanner,
		":" + RoleVerifier,
	} {
		err = errors.Join(err, r.stores.DiscardFiles(envID+suffix))
	}
	return err
}

func (r *runner) discardRootFiles(envID string) error {
	rootID := ""
	for _, task := range r.graph.Snapshot().Tasks {
		if task.Env.ID == envID {
			rootID = task.ID
			break
		}
	}
	if rootID == "" {
		return r.discardTaskFiles(envID)
	}
	var err error
	for _, taskID := range r.graph.taskTree(rootID) {
		task, ok := r.graph.Task(taskID)
		if ok {
			err = errors.Join(err, r.discardTaskFiles(task.Env.ID))
		}
	}
	return err
}

type joinedTask struct {
	task Task
	out  string
}

func (r *runner) projectCandidateTaskReport(_ Task, child Task, output string) error {
	if err := r.stores.ProjectCandidateTaskReport(child, output); err != nil {
		return fmt.Errorf("%w for task %s: %w", errTaskReportProjection, child.ID, err)
	}
	return nil
}

func roleWorkspaceID(task Task, nodeID string) string {
	switch nodeID {
	case task.Planner.ID:
		return task.Env.ID + ":" + RolePlanner
	case task.Verifier.ID:
		return task.Env.ID + ":" + RoleVerifier
	default:
		return task.Env.ID
	}
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
	progress := TaskProgress{
		Outputs: copied, Merged: mergedIDs, Prepared: prepared,
	}
	if ok {
		progress.Joins = current.Joins
	}
	if err := r.progress.Save(taskID, progress); err != nil {
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
	var err error
	ids := r.graph.taskTree(rootID)
	if r.progress != nil {
		for _, id := range ids {
			err = errors.Join(err, r.progress.Delete(id))
		}
	}
	if r.join != nil {
		r.join.forget(ids)
	}
	return err
}
