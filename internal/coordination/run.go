package coordination

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

const maxAutomaticRoleRecoveries = 2

var (
	ErrUnknownTask = errors.New("coordination: unknown task")
	ErrNilAssemble = errors.New("coordination: nil assemble")
	ErrNilAsker    = errors.New("coordination: nil asker")
	ErrNilStore    = errors.New("coordination: nil store")
	ErrRoleStalled = errors.New("coordination: role stalled")
	ErrTaskHeld    = errors.New("coordination: task is held")
)

// SetRunContext gives automatically started prerequisites the session lifetime.
// Canceling a consumer never cancels another task that is producing its input.
func (g *Graph) SetRunContext(ctx context.Context) {
	if ctx == nil {
		panic("nil context")
	}
	g.mu.Lock()
	g.runContext = ctx
	g.mu.Unlock()
}

// CancelTask stops only this task's current activation.
func (g *Graph) CancelTask(taskID string) {
	g.mu.Lock()
	r := g.runners[taskID]
	g.mu.Unlock()
	if r != nil {
		r.cancel()
	}
}

// WaitRuns waits for actual executions, including prerequisites started by an edge.
// Call after canceling the session and stopping new submissions.
func (g *Graph) WaitRuns() { g.runWG.Wait() }

func (g *Graph) Run(ctx context.Context, taskID, input string, stores Stores, assemble AssembleFunc) (string, error) {
	return g.RunWithReport(ctx, taskID, input, stores, assemble, nil)
}

// RunWithReport shares an activation with concurrent callers. Report delivery is
// separate from execution and must succeed before the task leaves active state.
func (g *Graph) RunWithReport(ctx context.Context, taskID, input string, stores Stores, assemble AssembleFunc, report func(Task, string, error) error) (string, error) {
	r, owner, err := g.start(ctx, taskID, input, stores, assemble, report)
	if err != nil {
		return "", err
	}
	registered := owner || report == nil || r.addReport(report)
	if owner || report != nil {
		<-r.done
	} else {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.done:
		}
	}
	if !registered {
		if err := r.deliverReport(report, r.result.output, r.result.err); err != nil {
			return "", errors.Join(r.result.err, err, g.setTaskActive(taskID, r.task.Activation))
		}
	}
	return r.result.output, r.result.err
}

func (g *Graph) start(ctx context.Context, taskID, input string, stores Stores, assemble AssembleFunc, report func(Task, string, error) error) (*runner, bool, error) {
	if ctx == nil {
		panic("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if assemble == nil {
		return nil, false, ErrNilAssemble
	}
	if stores.Memory == nil {
		return nil, false, ErrNilStore
	}
	g.mu.Lock()
	if r := g.runners[taskID]; r != nil {
		g.mu.Unlock()

		return r, false, nil
	}
	task, ok := g.taskByIDLocked(taskID)
	if !ok {
		g.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %q", ErrUnknownTask, taskID)
	}
	if task.RealDirectory && g.projectTaskID != task.ID && task.Outcome != OutcomeDone {
		g.mu.Unlock()
		return nil, false, fmt.Errorf("coordination: real directory belongs to a newer task")
	}
	if task.Outcome == OutcomeClosed {
		g.mu.Unlock()
		return nil, false, fmt.Errorf("coordination: task %s is closed", taskID)
	}
	if task.RunPolicy == RunPolicyHeld {
		g.mu.Unlock()
		return nil, false, fmt.Errorf("%w: %s", ErrTaskHeld, taskID)
	}
	if g.progress == nil {
		g.progress = &memoryProgressStore{}
	}
	runCtx, cancel := context.WithCancel(ctx)
	r := &runner{
		graph: g, task: task, stores: stores, assemble: assemble, progress: g.progress,
		ctx: runCtx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}),
		nodeStarted: make(map[string]struct{}), state: TaskProgress{Version: progressVersion},
	}
	if report != nil {
		r.reports = append(r.reports, report)
	}
	r.inputs = &inputCoordinator{graph: g, runner: r}
	if g.runners == nil {
		g.runners = make(map[string]*runner)
	}
	g.runners[taskID] = r
	g.runWG.Add(1)
	g.mu.Unlock()
	go r.execute(input)
	return r, true, nil
}

type taskResult struct {
	output string
	err    error
}

type runner struct {
	graph       *Graph
	task        Task
	stores      Stores
	assemble    AssembleFunc
	progress    ProgressStore
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	result      taskResult
	mu          sync.Mutex
	changed     chan struct{}
	nodeStarted map[string]struct{}
	state       TaskProgress
	inputs      *inputCoordinator
	roles       Roles
	exportMu    sync.Mutex
	reportMu    sync.Mutex
	reports     []func(Task, string, error) error
	reported    bool
	// Protected by graph.mu with message acceptance and the final inbox check.
	projectNode string
	projectSeen int
}

func (r *runner) execute(input string) {
	defer r.graph.runWG.Done()
	defer r.cancel()
	output, executionErr := r.runTask(input)
	// Execution survives failed report delivery. Only the activation's own error
	// determines its outcome; failure elsewhere never propagates through creation.
	var reportErr error
	for i := 0; ; i++ {
		r.reportMu.Lock()
		if i == len(r.reports) {
			r.reported = true
			r.reportMu.Unlock()
			break
		}
		report := r.reports[i]
		r.reportMu.Unlock()
		reportErr = errors.Join(reportErr, r.deliverReport(report, output, executionErr))
	}
	err := errors.Join(executionErr, reportErr)
	if reportErr == nil {
		err = errors.Join(err, r.graph.recordOutcome(r.task.ID, executionErr))
	}
	r.result = taskResult{output: output, err: err}
	r.graph.mu.Lock()
	delete(r.graph.runners, r.task.ID)
	close(r.done)
	r.graph.mu.Unlock()
}

func (r *runner) addReport(report func(Task, string, error) error) bool {
	r.reportMu.Lock()
	defer r.reportMu.Unlock()
	if r.reported {
		return false
	}
	r.reports = append(r.reports, report)
	return true
}

func (r *runner) deliverReport(report func(Task, string, error) error, output string, err error) error {
	task := r.task
	task.Outcome = taskOutcome(task, err)
	return report(task, output, err)
}

func (r *runner) runTask(input string) (output string, err error) {
	if r.progress != nil {
		state, ok, loadErr := r.progress.Load(r.task.Env.ID)
		if loadErr != nil {
			return "", loadErr
		}
		if ok {
			r.state = state
		}
	}
	if r.state.Pending != nil {
		if err := r.completeExport(*r.state.Pending); err != nil {
			return "", err
		}
	}
	if output, ok := r.graph.Output(r.task.Verifier.ID); ok {
		return output.Report, nil
	}
	if err := r.ctx.Err(); err != nil {
		return "", err
	}
	roles, err := r.assemble(r.task)
	if err != nil {
		return "", err
	}
	r.roles = roles
	defer func() {
		for _, workspace := range []string{r.task.Env.ID, r.task.Env.ID + ":" + RolePlanner, r.task.Env.ID + ":" + RoleVerifier} {
			if r.stores.Exec != nil {
				if reapErr := r.stores.Exec.Reap(workspace); reapErr != nil {
					err = errors.Join(err, reapErr)
					continue
				}
			}
			if r.stores.Files != nil {
				err = errors.Join(err, r.stores.Files.Release(workspace))
			}
		}
	}()
	output = input
	for _, node := range r.task.Sequence() {
		if err := r.ctx.Err(); err != nil {
			return "", err
		}
		if saved, ok := r.graph.Output(node.ID); ok {
			output = saved.Report
			continue
		}
		output, err = r.runRole(node, output)
		if err != nil {
			return "", err
		}
	}
	return output, nil
}

func (r *runner) runRole(node Node, input string) (string, error) {
	r.markNodeStarted(node.ID)
	asker := r.roles.asker(node.Role)
	if asker == nil {
		return "", fmt.Errorf("%w: %s", ErrNilAsker, node.Role)
	}
	sources, err := r.collectInputs(r.ctx, node)
	if err != nil {
		return "", err
	}
	ready, err := r.prepareInput(r.ctx, node, sources)
	if err != nil {
		return "", err
	}
	workspace := roleWorkspaceID(r.task, node.ID)
	if r.roles.bind == nil {
		workspace = r.task.Env.ID
	}
	ready = r.latestRoleInput(node.ID, ready)
	if err := r.installInput(&ready, workspace); err != nil {
		return "", err
	}
	if r.roles.bind != nil {
		if err := r.roles.bind(node.Role, r.task.Env.ID, workspace); err != nil {
			return "", err
		}
	}
	if len(sources) > 1 {
		input = "输入已准备，请依据当前文件与任务记忆继续。"
	} else if len(sources) == 1 && sources[0].Report != "" {
		input = sources[0].Report
	}
	query := taskInput(r.task.Info, input)
	if r.task.RealDirectory {
		query = "[真实目录工作区] 当前 task 的文件和命令工具直接作用于真实项目目录，改动立即可见。已有内容已作为输入来源完成合入。Planner/Verifier 仍不修复实现；临时实验也会影响真实目录，必须自行清理。运行中可用 coordination_messageManager 向 manager 发送进展、问题或答复；manager 消息会在模型请求前送达。消息不改变角色职责，不是用户授权或验收结论。\n\n" + query
	}
	output, err := r.askProjectRole(node, asker, query)
	if err != nil {
		return "", err
	}
	if r.stores.Exec != nil {
		if err := r.stores.Exec.Reap(workspace); err != nil {
			return "", err
		}
	}
	filesID := workspace
	memory := r.stores.Memory.Load(r.task.Env.ID)
	if node.Role != RoleExecutor && r.roles.bind != nil && !r.task.RealDirectory {
		ready = r.latestRoleInput(node.ID, ready)
		filesID = ready.FilesRef
		memory, err = r.qualifyDisposableMemory(node, workspace, ready, memory)
		if err != nil {
			return "", err
		}
	}
	if err := r.export(node, filesID, memory, output); err != nil {
		return "", err
	}
	if r.stores.Files != nil && node.Role != RoleExecutor && r.roles.bind != nil && !r.task.RealDirectory {
		if err := r.stores.DiscardFiles(workspace); err != nil {
			return "", err
		}
	}
	if r.task.RealDirectory && r.stores.Files != nil {
		if err := r.stores.Files.Freeze(workspace); err != nil {
			return "", err
		}
	}
	return output, nil
}

func (r *runner) export(node Node, filesID string, memory ctxgraph.Graph, report string) error {
	r.exportMu.Lock()
	defer r.exportMu.Unlock()
	if _, ok := r.graph.Output(node.ID); ok {
		return nil
	}
	r.mu.Lock()
	pending := cloneTaskProgress(r.state).Pending
	r.mu.Unlock()
	if pending != nil {
		return r.completeExport(*pending)
	}
	output := Output{Node: node, FilesRef: "no-files:" + node.ID, MemoryRef: node.ID + ":memory", Report: report}
	if r.stores.Files != nil {
		output.FilesRef = node.ID + ":files"
		// An unjournaled archive is an orphan, never a published source. Replacing
		// it is safe; once journaled, retries keep this exact pair.
		if err := r.stores.Files.Archive(filesID, output.FilesRef); err != nil {
			return err
		}
	}
	export := ExportProgress{Output: output, Memory: memory.Clone()}
	if err := r.updateProgress(func(state *TaskProgress) { state.Pending = &export }); err != nil {
		return err
	}
	return r.completeExport(export)
}

func (r *runner) completeExport(export ExportProgress) error {
	if r.stores.Files != nil {
		if err := r.stores.Files.Restore(export.Output.FilesRef); err != nil {
			return err
		}
	}
	if err := r.stores.Memory.SaveSnapshot(export.Output.MemoryRef, export.Memory); err != nil {
		return err
	}
	if err := r.graph.commitOutput(export.Output); err != nil {
		return err
	}
	if err := r.updateProgress(func(state *TaskProgress) { state.Pending = nil }); err != nil {
		return err
	}
	r.mu.Lock()
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	return nil
}

func (r *runner) collectInputs(ctx context.Context, node Node) ([]Output, error) {
	incoming := r.graph.Incoming(node.ID)
	batch, exists := r.inputState(node.ID)
	var project *Output
	if r.task.RealDirectory && node.ID == r.task.Planner.ID {
		source, err := r.projectSource(node, !exists)
		if err != nil {
			return nil, err
		}
		project = &source
	}
	if exists {
		incoming = nil
		for _, source := range batch.Sources {
			if r.task.RealDirectory && node.ID == r.task.Planner.ID && source.ID == projectSourceID(node) {
				continue
			}
			output, ok := r.graph.Output(source.ID)
			if !ok || output.FilesRef != source.FilesRef || output.MemoryRef != source.MemoryRef {
				return nil, fmt.Errorf("input: committed source %s is missing or changed", source.ID)
			}
			incoming = append(incoming, output.Node)
		}
	}
	// Start the entire frontier before waiting on its ordered results. Otherwise
	// the first unfinished source would serialize all the remaining sources.
	for _, source := range incoming {
		if _, ready := r.graph.Output(source.ID); ready || source.TaskID == r.task.ID {
			continue
		}
		if _, err := r.startSource(ctx, source); err != nil && !errors.Is(err, ErrTaskHeld) {
			return nil, err
		}
	}
	outputs := make([]Output, 0, len(incoming))
	for _, source := range incoming {
		output, err := r.waitOutput(ctx, source)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, output)
	}
	if project != nil {
		outputs = append(outputs, *project)
	}
	return outputs, nil
}

func (r *runner) startSource(ctx context.Context, node Node) (*runner, error) {
	r.graph.mu.Lock()
	ownerCtx := r.graph.runContext
	r.graph.mu.Unlock()
	if ownerCtx == nil {
		ownerCtx = ctx
	}
	source, _, err := r.graph.start(ownerCtx, node.TaskID, "", r.stores, r.assemble, nil)
	return source, err
}

func (r *runner) waitOutput(ctx context.Context, node Node) (Output, error) {
	for {
		if output, ok := r.graph.Output(node.ID); ok {
			return output, nil
		}
		if node.TaskID == r.task.ID {
			return Output{}, fmt.Errorf("coordination: source %s has no committed output", node.ID)
		}
		r.graph.mu.Lock()
		task, exists := r.graph.taskByIDLocked(node.TaskID)
		changedGraph := r.graph.changed
		r.graph.mu.Unlock()
		if exists && task.RunPolicy == RunPolicyHeld && task.Outcome != OutcomeClosed {
			select {
			case <-ctx.Done():
				return Output{}, ctx.Err()
			case <-changedGraph:
				continue
			}
		}
		source, err := r.startSource(ctx, node)
		if errors.Is(err, ErrTaskHeld) {
			continue
		}
		if err != nil {
			return Output{}, err
		}
		source.mu.Lock()
		changed := source.changed
		source.mu.Unlock()
		if output, ok := r.graph.Output(node.ID); ok {
			return output, nil
		}
		select {
		case <-ctx.Done():
			return Output{}, ctx.Err()
		case <-changed:
		case <-source.done:
			if output, ok := r.graph.Output(node.ID); ok {
				return output, nil
			}
			return Output{}, fmt.Errorf("coordination: source %s did not produce an output: %w", node.ID, source.result.err)
		}
	}
}

func (g *Graph) runnerForNode(nodeID string) *runner {
	g.mu.Lock()
	defer g.mu.Unlock()
	node, ok := g.nodeByIDLocked(nodeID)
	if !ok {
		return nil
	}
	return g.runners[node.TaskID]
}

func (r *runner) markNodeStarted(nodeID string) {
	r.mu.Lock()
	r.nodeStarted[nodeID] = struct{}{}
	r.mu.Unlock()
}

func (r *runner) executionSnapshot() (map[string]struct{}, map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := make(map[string]struct{}, len(r.nodeStarted))
	for id := range r.nodeStarted {
		nodes[id] = struct{}{}
	}
	return map[string]struct{}{r.task.ID: {}}, nodes
}

func (r *runner) updateProgress(update func(*TaskProgress)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := cloneTaskProgress(r.state)
	update(&next)
	if r.progress != nil {
		if err := r.progress.Save(r.task.Env.ID, next); err != nil {
			return fmt.Errorf("saving activation progress: %w", err)
		}
	}
	r.state = next
	return nil
}

func cloneTaskProgress(state TaskProgress) TaskProgress {
	state.Inputs = cloneInputProgresses(state.Inputs)
	if state.Pending != nil {
		pending := *state.Pending
		pending.Memory = pending.Memory.Clone()
		state.Pending = &pending
	}
	return state
}

func (g *Graph) recordOutcome(taskID string, err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	for i := range g.tasks {
		if g.tasks[i].ID == taskID && g.tasks[i].Outcome != OutcomeClosed {
			g.tasks[i].Outcome = taskOutcome(g.tasks[i], err)
		}
	}
	return g.saveOrRestoreLocked(before)
}

func (g *Graph) setTaskActive(taskID string, activation uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.stateLocked()
	for i := range g.tasks {
		if g.tasks[i].ID == taskID && g.tasks[i].Activation == activation && g.tasks[i].Outcome != OutcomeClosed {
			g.tasks[i].Outcome = OutcomeActive
		}
	}
	return g.saveOrRestoreLocked(before)
}

func taskOutcome(task Task, err error) string {
	if err == nil && task.Persistent {
		return OutcomeIdle
	}
	return outcomeForError(err)
}

func outcomeForError(err error) string {
	if err == nil {
		return OutcomeDone
	}
	if canceledOnly(err) {
		return OutcomeCanceled
	}
	return OutcomeFailed
}

func canceledOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !canceledOnly(child) {
				return false
			}
		}
		return len(joined.Unwrap()) > 0
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		return canceledOnly(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled)
}

func askRole(ctx context.Context, asker Asker, input string) (string, error) {
	for recoveries := 0; ; recoveries++ {
		output, err := asker.Ask(ctx, input)
		if err == nil || !agent.IsRecoverableTurnError(err) {
			return output, err
		}
		if recoveries >= maxAutomaticRoleRecoveries {
			return "", fmt.Errorf("%w after %d automatic recoveries: %w", ErrRoleStalled, recoveries, err)
		}
	}
}

func taskInput(info, upstream string) string {
	if info == "" {
		return upstream
	}
	if upstream == "" {
		return info
	}
	return info + "\n\n" + upstream
}

func roleWorkspaceID(task Task, nodeID string) string {
	if nodeID == task.Planner.ID {
		return task.Env.ID + ":" + RolePlanner
	}
	if nodeID == task.Verifier.ID {
		return task.Env.ID + ":" + RoleVerifier
	}
	return task.Env.ID
}
