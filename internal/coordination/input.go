package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"reflect"
	"slices"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func (r *runner) inputState(nodeID string) (InputProgress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, input := range cloneInputProgresses(r.state.Inputs) {
		if input.NodeID == nodeID {
			return input, true
		}
	}
	return InputProgress{}, false
}

func (r *runner) saveInput(input InputProgress) error {
	return r.updateProgress(func(state *TaskProgress) {
		for i := range state.Inputs {
			if state.Inputs[i].ID == input.ID {
				state.Inputs[i] = input
				return
			}
		}
		state.Inputs = append(state.Inputs, input)
	})
}

// A Help resume replaces the role's retained input, including on restart.
func (r *runner) latestRoleInput(nodeID string, initial InputProgress) InputProgress {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.state.Inputs) - 1; i >= 0; i-- {
		input := r.state.Inputs[i]
		if input.Started && input.Phase == "ready" && (input.NodeID == nodeID || input.ResumeFor == nodeID) {
			return cloneInputProgresses([]InputProgress{input})[0]
		}
	}
	return initial
}

func (r *runner) installInput(input *InputProgress, workspace string) error {
	if input.Started {
		if r.stores.Files == nil {
			return nil
		}
		// Retain edits from an interrupted role. An unmaterialized workspace has
		// no disk state, so reconstruct it from its immutable input instead.
		err := r.stores.Files.Restore(workspace)
		if errors.Is(err, vfs.ErrUnknownEnvironment) {
			return r.stores.Files.CreateEnvironment(input.FilesRef, workspace)
		}
		return err
	}
	if err := r.stores.Memory.Restore(r.task.Env.ID, input.MemoryRef); err != nil {
		return err
	}
	if r.stores.Files != nil {
		if err := r.stores.DiscardFiles(workspace); err != nil {
			return err
		}
		if err := r.stores.Files.CreateEnvironment(input.FilesRef, workspace); err != nil {
			return err
		}
	}
	input.Started = true
	return r.saveInput(*input)
}

// prepareInput handles every ordinary edge, including role input and Help.
// Source snapshots are never installed into live role memory before ready.
func (r *runner) prepareInput(ctx context.Context, node Node, sources []Output) (ready InputProgress, retErr error) {
	input, exists := r.inputState(node.ID)
	if !exists {
		input = InputProgress{ID: "input:" + node.ID, NodeID: node.ID, TargetID: node.ID + ":input", Phase: "collecting"}
		for _, source := range sources {
			input.Sources = append(input.Sources, InputSourceProgress{
				ID: source.Node.ID, FilesRef: source.FilesRef, MemoryRef: source.MemoryRef, Output: source.Report,
			})
		}
		if err := r.saveInput(input); err != nil {
			return InputProgress{}, err
		}
	} else {
		if len(sources) != len(input.Sources) {
			return InputProgress{}, fmt.Errorf("input: source batch changed for %s", node.ID)
		}
		for i, source := range sources {
			if source.Node.ID != input.Sources[i].ID || source.FilesRef != input.Sources[i].FilesRef || source.MemoryRef != input.Sources[i].MemoryRef {
				return InputProgress{}, fmt.Errorf("input: source snapshot changed for %s", node.ID)
			}
		}
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, r.releaseInputWorkspace(input.TargetID))
		}
	}()
	if input.Phase == "ready" {
		return r.readyInput(input)
	}
	if err := ctx.Err(); err != nil {
		return InputProgress{}, err
	}
	if input.Phase == "collecting" || input.Phase == "files" {
		refs := make([]string, 0, len(input.Sources))
		for _, source := range input.Sources {
			refs = append(refs, source.FilesRef)
		}
		if r.stores.Files != nil {
			if len(refs) == 0 {
				if err := r.stores.Files.CreateEnvironment(r.task.Env.ID, input.TargetID); err != nil {
					return InputProgress{}, err
				}
			} else {
				prepared, err := r.stores.Files.PrepareInput(input.TargetID, refs)
				if err != nil {
					return InputProgress{}, err
				}
				input.Paths = prepared.Paths
				for i, candidate := range prepared.Candidates {
					input.Sources[i].EnvID = candidate
				}
			}
		}
		input.Phase = "files"
		if len(input.Paths) == 0 {
			input.Phase = "files_resolved"
			for i := range input.Sources {
				input.Sources[i].AppliedAll = true
			}
		}
		if err := r.saveInput(input); err != nil {
			return InputProgress{}, err
		}
		if input.Phase == "files" {
			if r.roles.ResolveInput == nil {
				return InputProgress{}, fmt.Errorf("input: file resolver is required for %s", node.ID)
			}
			if err := r.roles.ResolveInput(ctx, node, input); err != nil {
				return InputProgress{}, err
			}
			input, _ = r.inputState(node.ID)
			if input.Phase != "files_resolved" {
				return InputProgress{}, fmt.Errorf("input: file resolver returned before finish for %s", node.ID)
			}
		}
	}
	if input.Phase == "files_resolved" {
		if r.stores.Exec != nil {
			if err := r.stores.Exec.Reap(input.TargetID); err != nil {
				return InputProgress{}, err
			}
		}
		input.FilesRef = "no-files:" + input.ID
		if r.stores.Files != nil {
			input.FilesRef = input.ID + ":files"
			changes, err := r.stores.Files.InputChanges(input.TargetID)
			if err != nil {
				return InputProgress{}, err
			}
			for _, change := range changes {
				input.Paths = appendUnique(input.Paths, change.Path)
			}
			if err := r.freezeFiles(input.TargetID, input.FilesRef); err != nil {
				return InputProgress{}, err
			}
		}
		input.Phase = "memory"
		if err := r.saveInput(input); err != nil {
			return InputProgress{}, err
		}
	}
	memorySources := make([]ctxgraph.InputSource, 0, len(input.Sources))
	for _, source := range input.Sources {
		graph, ok := r.stores.Memory.Snapshot(source.MemoryRef)
		if !ok {
			return InputProgress{}, fmt.Errorf("input: missing memory snapshot %s", source.MemoryRef)
		}
		memorySources = append(memorySources, ctxgraph.InputSource{Ref: source.MemoryRef, Graph: graph})
	}
	if _, ok := r.stores.Memory.Snapshot(input.ID + ":memory"); ok {
		input.MemoryRef = input.ID + ":memory"
		input.Phase = "ready"
		if err := r.saveInput(input); err != nil {
			return InputProgress{}, err
		}
		return r.readyInput(input)
	}
	partition := ctxgraph.PartitionInputs(memorySources)
	memory := partition.Common
	if len(memorySources) == 0 {
		memory = r.stores.Memory.Load(r.task.Env.ID)
	}
	if partition.HasDifferences() {
		if r.roles.OrganizeMemory == nil {
			return InputProgress{}, fmt.Errorf("input: memory organizer is required for %s", node.ID)
		}
		evidence, err := r.fileEvidence(input)
		if err != nil {
			return InputProgress{}, err
		}
		filesRef := input.FilesRef
		if filesRef == "" {
			filesRef = "no-files:" + input.ID
		}
		memory, err = r.roles.OrganizeMemory(ctx, agent.InputMemoryRequest{Sources: memorySources, FilesRef: filesRef, Evidence: evidence})
		if err != nil {
			return InputProgress{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return InputProgress{}, err
	}
	input.MemoryRef = input.ID + ":memory"
	if err := r.stores.Memory.SaveSnapshot(input.MemoryRef, memory); err != nil {
		return InputProgress{}, err
	}
	input.Phase = "ready"
	if err := r.saveInput(input); err != nil {
		return InputProgress{}, err
	}
	return r.readyInput(input)
}

func (r *runner) releaseInputWorkspace(id string) error {
	var err error
	if r.stores.Exec != nil {
		err = r.stores.Exec.Reap(id)
	}
	if r.stores.Files != nil {
		err = errors.Join(err, r.stores.Files.Freeze(id))
	}
	return err
}

func (r *runner) readyInput(input InputProgress) (InputProgress, error) {
	if _, err := r.validateReady(input); err != nil {
		return InputProgress{}, err
	}
	var err error
	for _, source := range input.Sources {
		if source.EnvID != "" {
			err = errors.Join(err, r.stores.DiscardFiles(source.EnvID))
		}
	}
	err = errors.Join(err, r.stores.DiscardFiles(input.TargetID))
	return input, err
}

func (r *runner) validateReady(input InputProgress) (InputProgress, error) {
	if _, ok := r.stores.Memory.Snapshot(input.MemoryRef); !ok {
		return InputProgress{}, fmt.Errorf("input: ready memory is missing: %s", input.MemoryRef)
	}
	if r.stores.Files != nil && input.FilesRef == "" {
		return InputProgress{}, fmt.Errorf("input: ready file reference is missing")
	}
	if r.stores.Files != nil {
		if err := r.stores.Files.Restore(input.FilesRef); err != nil {
			return InputProgress{}, err
		}
	}
	return input, nil
}

func (r *runner) freezeFiles(source, ref string) error {
	if err := r.stores.Files.Restore(ref); err == nil {
		return nil
	} else if !errors.Is(err, vfs.ErrUnknownEnvironment) {
		return err
	}
	return r.stores.Files.Archive(source, ref)
}

// Evidence is bounded and explicitly records truncation. Unseen content never
// becomes proof that a candidate claim holds in the selected file state.
func (r *runner) fileEvidence(input InputProgress) (string, error) {
	if r.stores.Files == nil {
		return "", nil
	}
	paths := slices.Clone(input.Paths)
	changes, err := r.stores.Files.InputChanges(input.FilesRef)
	if err != nil {
		return "", err
	}
	for _, change := range changes {
		paths = appendUnique(paths, change.Path)
	}
	type observation struct {
		Path      string `json:"path"`
		Exists    bool   `json:"exists"`
		Directory bool   `json:"directory,omitempty"`
		Size      int64  `json:"size,omitempty"`
		SHA256    string `json:"sha256,omitempty"`
		Content   string `json:"content,omitempty"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	result := struct {
		FilesRef     string                `json:"files_ref"`
		Reason       string                `json:"reason"`
		Decisions    []InputSourceProgress `json:"decisions"`
		Observations []observation         `json:"observations"`
		OmittedPaths int                   `json:"omitted_paths,omitempty"`
	}{FilesRef: input.FilesRef, Reason: input.Reason}
	for _, source := range input.Sources {
		source.Output = ""
		result.Decisions = append(result.Decisions, source)
	}
	budget := 24000
	for i, path := range paths {
		if budget <= 0 || len(result.Observations) >= 100 {
			result.OmittedPaths = len(paths) - i
			break
		}
		item := observation{Path: path}
		info, err := r.stores.Files.View(input.FilesRef).Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			result.Observations = append(result.Observations, item)
			continue
		}
		if err != nil {
			return "", err
		}
		item.Exists, item.Directory, item.Size = true, info.IsDir, info.Size
		if !info.IsDir {
			data, err := r.stores.Files.View(input.FilesRef).Read(path)
			if err != nil {
				return "", err
			}
			item.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
			limit := min(len(data), min(4000, budget))
			item.Content = string(data[:limit])
			item.Truncated = limit < len(data)
			budget -= limit
		}
		result.Observations = append(result.Observations, item)
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func (r *runner) qualifyDisposableMemory(node Node, workspace string, input InputProgress, memory ctxgraph.Graph) (ctxgraph.Graph, error) {
	if r.stores.Files == nil {
		return memory, nil
	}
	changes, err := r.stores.Files.InputChanges(workspace)
	if err != nil || len(changes) == 0 {
		return memory, err
	}
	baseline := r.stores.Memory.Load(input.MemoryRef)
	original := make(map[string]ctxgraph.Node, len(baseline.Nodes))
	for _, item := range baseline.Nodes {
		original[item.ID] = item
	}
	ref := node.ID + ":observed-files"
	// A prior unjournaled attempt may have left an orphan at this reference.
	// Once journaled, export recovery skips the role and preserves that archive.
	if err := r.stores.Files.Archive(workspace, ref); err != nil {
		return ctxgraph.Graph{}, err
	}
	for i, item := range memory.Nodes {
		if item.Kind == ctxgraph.NodeKindFact && !reflect.DeepEqual(original[item.ID], item) {
			memory.Nodes[i].Status = ctxgraph.NodeStatusDisputed
			memory.Nodes[i].SourceRefs = appendUnique(item.SourceRefs, ref)
		}
	}
	return memory, nil
}

func (r *runner) pauseHelp(nodeID, callID string) (Node, Node, error) {
	original, ok := r.graph.taskForNode(nodeID)
	if !ok || original.ID != r.task.ID {
		return Node{}, Node{}, fmt.Errorf("help: unknown requester %s", nodeID)
	}
	role := ""
	for _, node := range r.task.Sequence() {
		if node.ID == nodeID {
			role = node.Role
		}
	}
	if role == "" {
		return Node{}, Node{}, fmt.Errorf("help: requester is not a current role")
	}
	pause := Node{ID: nodeID + ":pause:" + url.PathEscape(callID), TaskID: r.task.ID, Role: role}
	resume := Node{ID: nodeID + ":resume:" + url.PathEscape(callID), TaskID: r.task.ID, Role: role}
	for _, node := range []Node{pause, resume} {
		if err := r.graph.addCheckpointNode(node); err != nil {
			return Node{}, Node{}, err
		}
	}
	// A pause is partial state, so it does not depend on the unfinished role's
	// final output. The role's completion does depend on its resume point.
	r.graph.mu.Lock()
	before := r.graph.stateLocked()
	addEdge := func(from, to string) {
		for _, edge := range r.graph.edges {
			if edge.From == from && edge.To == to {
				return
			}
		}
		r.graph.edges = append(r.graph.edges, Edge{From: from, To: to})
	}
	for _, edge := range before.Edges {
		if edge.To == nodeID && edge.From != resume.ID {
			addEdge(edge.From, pause.ID)
		}
	}
	addEdge(resume.ID, nodeID)
	err := r.graph.saveOrRestoreLocked(before)
	r.graph.mu.Unlock()
	if err != nil {
		return Node{}, Node{}, err
	}
	if _, exists := r.graph.Output(pause.ID); !exists {
		workspace := roleWorkspaceID(r.task, nodeID)
		if r.roles.bind == nil {
			workspace = r.task.Env.ID
		}
		filesID := workspace
		memory := r.stores.Memory.Load(r.task.Env.ID)
		if role != RoleExecutor && r.roles.bind != nil {
			input := r.latestRoleInput(nodeID, InputProgress{})
			filesID = input.FilesRef
			memory, err = r.qualifyDisposableMemory(pause, workspace, input, memory)
			if err != nil {
				return Node{}, Node{}, err
			}
		}
		if err := r.export(pause, filesID, memory, "当前角色暂停时的可保留状态"); err != nil {
			return Node{}, Node{}, err
		}
	}
	return pause, resume, nil
}

func (r *runner) resumeHelp(ctx context.Context, nodeID, resumeID string) (string, error) {
	r.graph.mu.Lock()
	node, ok := r.graph.nodeByIDLocked(resumeID)
	r.graph.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("help: resume node missing")
	}
	r.markNodeStarted(resumeID)
	sources, err := r.collectInputs(ctx, node)
	if err != nil {
		return "", err
	}
	ready, err := r.prepareInput(ctx, node, sources)
	if err != nil {
		return "", err
	}
	workspace := roleWorkspaceID(r.task, nodeID)
	if r.roles.bind == nil {
		workspace = r.task.Env.ID
	}
	ready.ResumeFor = nodeID
	if err := r.installInput(&ready, workspace); err != nil {
		return "", err
	}
	// Record the resumed pair without including candidate dialogue in the live role.
	if err := r.export(node, ready.FilesRef, r.stores.Memory.Load(ready.MemoryRef), "帮助输入已准备"); err != nil {
		return "", err
	}
	if r.roles.bind != nil {
		if err := r.roles.bind(node.Role, r.task.Env.ID, workspace); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("[input ready] session_id=%s。文件与记忆已配套提交；请依据当前状态继续。", ready.ID), nil
}
