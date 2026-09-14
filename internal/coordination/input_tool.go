package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"

	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

const inputToolName = "input"

type inputCoordinator struct {
	graph *Graph

	mu     sync.Mutex
	runner *runner
}

type inputTool struct{ graph *Graph }

func (t inputTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        inputToolName,
		Description: "处理固定输入批次的文件差异。共同文件已直接使用；检查来源、按路径采纳或丢弃差异，finish 结束文件阶段，然后运行时整理记忆。",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"action":{"type":"string","enum":["list","inspect","apply","discard","finish"]},
				"session_id":{"type":"string"},
				"source_id":{"type":"string"},
				"source_ids":{"type":"array","items":{"type":"string"}},
				"view":{"type":"string","enum":["summary","output","diff","file","compare"]},
				"path":{"type":"string"},
				"paths":{"type":"array","minItems":1,"items":{"type":"string"}},
				"all":{"type":"boolean"},
				"strategy":{"type":"string","enum":["safe","replace"]},
				"reason":{"type":"string"},
				"offset":{"type":"integer","minimum":0},
				"limit":{"type":"integer","minimum":1,"maximum":1000}
			},
			"required":["action"],
			"additionalProperties":false
		}`),
	}
}

func (t inputTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.graph == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unavailable", inputToolName)
	}
	var args inputArgs
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	if args.Offset < 0 || args.Limit < 0 || args.Limit > 1000 {
		return agenttool.Output{}, fmt.Errorf("%s: invalid pagination", inputToolName)
	}
	run := t.graph.runnerForNode(agenttool.AgentID(ctx))
	if run == nil {
		return agenttool.Output{}, fmt.Errorf("%s: caller is not running", inputToolName)
	}
	coordinator := run.inputs
	result, err := coordinator.execute(
		agenttool.AgentID(ctx),
		agenttool.EnvFromContext(ctx),
		args,
	)
	if err != nil {
		return agenttool.Output{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("%s: encode result: %w", inputToolName, err)
	}
	return agenttool.Output{Content: string(data), Details: data}, nil
}

type inputArgs struct {
	Action    string   `json:"action"`
	SessionID string   `json:"session_id"`
	SourceID  string   `json:"source_id"`
	SourceIDs []string `json:"source_ids"`
	View      string   `json:"view"`
	Path      string   `json:"path"`
	Paths     []string `json:"paths"`
	All       bool     `json:"all"`
	Strategy  string   `json:"strategy"`
	Reason    string   `json:"reason"`
	Offset    int      `json:"offset"`
	Limit     int      `json:"limit"`
}

func (j *inputCoordinator) execute(nodeID, targetID string, args inputArgs) (any, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("%s: caller agent is unknown", inputToolName)
	}
	taskID, ok := j.taskID(nodeID)
	if !ok {
		return nil, fmt.Errorf("%s: caller node %q is unknown", inputToolName, nodeID)
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	sessions, err := j.loadLocked(taskID)
	if err != nil {
		return nil, err
	}
	if args.Action == "list" {
		return j.listLocked(sessions, nodeID, targetID, args), nil
	}
	index, err := selectInputSession(sessions, nodeID, targetID, strings.TrimSpace(args.SessionID))
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, fmt.Errorf("%s: unknown session %q", inputToolName, args.SessionID)
	}
	session := &sessions[index]
	if session.NodeID != nodeID || session.TargetID != targetID {
		return nil, fmt.Errorf("%s: session %q does not belong to this role workspace", inputToolName, session.ID)
	}
	if inputFinished(*session) {
		if args.Action == "finish" {
			return map[string]any{"session_id": session.ID, "files_finished": true, "task_id": taskID}, nil
		}
		return nil, fmt.Errorf("%s: session %q is already finished", inputToolName, session.ID)
	}

	var result any
	switch args.Action {
	case "inspect":
		result, err = j.inspectLocked(*session, args)
	case "apply":
		result, err = j.applyLocked(session, args)
	case "discard":
		result, err = j.discardLocked(session, args)
	case "finish":
		result, err = j.finishLocked(taskID, session, args)
	default:
		err = fmt.Errorf("%s: unknown action %q", inputToolName, args.Action)
	}
	if err != nil {
		return nil, err
	}
	if args.Action != "inspect" {
		if err := j.saveLocked(taskID, sessions); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (j *inputCoordinator) listLocked(
	sessions []InputProgress,
	nodeID, targetID string,
	args inputArgs,
) any {
	type source struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Preview string `json:"output_preview,omitempty"`
	}
	type summary struct {
		ID         string   `json:"session_id"`
		Status     string   `json:"status"`
		Sources    []source `json:"sources"`
		NextOffset *int     `json:"next_offset"`
	}
	result := struct {
		Sessions []summary `json:"sessions"`
	}{Sessions: []summary{}}
	for _, session := range sessions {
		if session.NodeID != nodeID || session.TargetID != targetID {
			continue
		}
		status := "pending"
		if inputFinished(session) {
			status = "finished"
		}
		start, end, next := pageRange(len(session.Sources), args.Offset, args.Limit, 100)
		item := summary{
			ID: session.ID, Status: status,
			Sources: make([]source, 0, end-start), NextOffset: next,
		}
		for _, candidate := range session.Sources[start:end] {
			item.Sources = append(item.Sources, source{
				ID:      candidate.ID,
				Status:  inputSourceStatus(candidate),
				Preview: preview(candidate.Output, 300),
			})
		}
		result.Sessions = append(result.Sessions, item)
	}
	return result
}

func (j *inputCoordinator) inspectLocked(session InputProgress, args inputArgs) (any, error) {
	view := args.View
	if view == "" {
		view = "summary"
	}
	if view == "compare" {
		return j.compareLocked(session, args)
	}
	source, err := inputSource(&session, strings.TrimSpace(args.SourceID))
	if err != nil {
		return nil, err
	}
	changes, err := j.changesLocked(source.EnvID)
	if err != nil {
		return nil, err
	}
	switch view {
	case "summary":
		return map[string]any{
			"source_id":      source.ID,
			"status":         inputSourceStatus(*source),
			"output_preview": preview(source.Output, 1000),
			"changed_paths":  len(changes),
		}, nil
	case "output":
		output, next := pageText(source.Output, args.Offset, args.Limit)
		return map[string]any{"source_id": source.ID, "output": output, "next_offset": next}, nil
	case "diff":
		page, next := pageChanges(changes, args.Offset, args.Limit)
		return map[string]any{"source_id": source.ID, "changes": page, "next_offset": next}, nil
	case "file":
		path := strings.TrimSpace(args.Path)
		if path == "" {
			return nil, fmt.Errorf("%s: path is required for file inspection", inputToolName)
		}
		if !hasInputChange(changes, path) {
			return nil, fmt.Errorf("%s: path %q is not a candidate change", inputToolName, path)
		}
		if j.runner == nil || j.runner.stores.Files == nil {
			return nil, fmt.Errorf("%s: file store is unavailable", inputToolName)
		}
		data, readErr := j.runner.stores.Files.View(source.EnvID).Read(path)
		if errors.Is(readErr, fs.ErrNotExist) {
			return map[string]any{"source_id": source.ID, "path": path, "deleted": true}, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		content, next := pageText(string(data), args.Offset, args.Limit)
		return map[string]any{
			"source_id":   source.ID,
			"path":        path,
			"content":     content,
			"next_offset": next,
		}, nil
	default:
		return nil, fmt.Errorf("%s: unknown inspect view %q", inputToolName, view)
	}
}

func (j *inputCoordinator) compareLocked(session InputProgress, args inputArgs) (any, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return nil, fmt.Errorf("%s: path is required for compare", inputToolName)
	}
	if j.runner == nil || j.runner.stores.Files == nil {
		return nil, fmt.Errorf("%s: file store is unavailable", inputToolName)
	}
	changed := false
	for _, source := range session.Sources {
		changes, err := j.changesLocked(source.EnvID)
		if err != nil {
			return nil, err
		}
		changed = changed || hasInputChange(changes, path)
	}
	if !changed {
		return nil, fmt.Errorf("%s: path %q is not changed by this session", inputToolName, path)
	}
	result := map[string]any{
		"path":   path,
		"target": readInputFile(j.runner.stores.Files, session.TargetID, path, args.Offset, args.Limit),
	}
	sources := make(map[string]any, len(session.Sources))
	for _, source := range session.Sources {
		sources[source.ID] = readInputFile(
			j.runner.stores.Files,
			source.EnvID,
			path,
			args.Offset,
			args.Limit,
		)
	}
	result["sources"] = sources
	return result, nil
}

func (j *inputCoordinator) applyLocked(session *InputProgress, args inputArgs) (any, error) {
	source, err := inputSource(session, strings.TrimSpace(args.SourceID))
	if err != nil {
		return nil, err
	}
	strategy := args.Strategy
	if strategy == "" {
		strategy = "safe"
	}
	if strategy != "safe" && strategy != "replace" {
		return nil, fmt.Errorf("%s: unknown apply strategy %q", inputToolName, strategy)
	}
	if args.All == (len(args.Paths) > 0) {
		return nil, fmt.Errorf("%s: apply requires exactly one of paths or all=true", inputToolName)
	}
	if strategy == "replace" && strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: replace requires reason", inputToolName)
	}
	paths := args.Paths
	if args.All {
		paths = nil
	}
	var result vfs.InputApplyResult
	if j.runner != nil && j.runner.stores.Files != nil {
		result, err = j.runner.stores.Files.ApplyInput(
			source.EnvID,
			session.TargetID,
			paths,
			strategy == "replace",
		)
		if err != nil {
			return nil, err
		}
		if len(result.Conflicts) > 0 {
			return map[string]any{
				"source_id": source.ID,
				"strategy":  strategy,
				"applied":   []string{},
				"conflicts": result.Conflicts,
			}, nil
		}
	}
	source.Applied = true
	source.AppliedPaths = appendUnique(source.AppliedPaths, result.Applied...)
	changes, err := j.changesLocked(source.EnvID)
	if err != nil {
		return nil, err
	}
	source.AppliedAll = inputChangesCovered(changes, source.AppliedPaths)
	return map[string]any{
		"source_id": source.ID,
		"strategy":  strategy,
		"applied":   result.Applied,
		"status":    inputSourceStatus(*source),
	}, nil
}

func (j *inputCoordinator) discardLocked(session *InputProgress, args inputArgs) (any, error) {
	if len(args.SourceIDs) == 0 && strings.TrimSpace(args.SourceID) != "" {
		args.SourceIDs = []string{args.SourceID}
	}
	if len(args.SourceIDs) == 0 {
		return nil, fmt.Errorf("%s: source_ids is required", inputToolName)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: discard requires reason", inputToolName)
	}
	for _, sourceID := range args.SourceIDs {
		source, err := inputSource(session, strings.TrimSpace(sourceID))
		if err != nil {
			return nil, err
		}
		source.Discarded = true
		source.Reason = strings.TrimSpace(args.Reason)
	}
	return map[string]any{"discarded": args.SourceIDs}, nil
}

func (j *inputCoordinator) finishLocked(taskID string, session *InputProgress, args inputArgs) (any, error) {
	if strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: finish requires reason", inputToolName)
	}
	undecided := make([]string, 0)
	for _, source := range session.Sources {
		if !source.AppliedAll && !source.Discarded {
			undecided = append(undecided, source.ID)
		}
	}
	if len(undecided) > 0 {
		return nil, fmt.Errorf("%s: undecided sources: %s", inputToolName, strings.Join(undecided, ", "))
	}
	session.Phase = "files_resolved"
	session.Reason = strings.TrimSpace(args.Reason)
	return map[string]any{"session_id": session.ID, "files_finished": true, "task_id": taskID}, nil
}

func inputFinished(session InputProgress) bool {
	return session.Phase == "files_resolved" || session.Phase == "memory" || session.Phase == "ready"
}

func (j *inputCoordinator) loadLocked(taskID string) ([]InputProgress, error) {
	if j.runner == nil || j.runner.task.ID != taskID {
		return nil, fmt.Errorf("input: unknown activation")
	}
	j.runner.mu.Lock()
	defer j.runner.mu.Unlock()
	return cloneInputProgresses(j.runner.state.Inputs), nil
}

func (j *inputCoordinator) saveLocked(taskID string, sessions []InputProgress) error {
	if j.runner == nil || j.runner.task.ID != taskID {
		return fmt.Errorf("input: unknown activation")
	}
	return j.runner.updateProgress(func(state *TaskProgress) { state.Inputs = cloneInputProgresses(sessions) })
}

func (j *inputCoordinator) changesLocked(envID string) ([]vfs.InputChange, error) {
	if j.runner == nil || j.runner.stores.Files == nil {
		return []vfs.InputChange{}, nil
	}
	return j.runner.stores.Files.InputChanges(envID)
}

func (j *inputCoordinator) taskID(nodeID string) (string, bool) {
	if j.graph == nil {
		return "", false
	}
	j.graph.mu.Lock()
	defer j.graph.mu.Unlock()
	node, ok := j.graph.nodeByIDLocked(nodeID)
	return node.TaskID, ok
}

func selectInputSession(
	sessions []InputProgress,
	nodeID, targetID, sessionID string,
) (int, error) {
	if sessionID != "" {
		return slices.IndexFunc(sessions, func(session InputProgress) bool {
			return session.ID == sessionID
		}), nil
	}
	index := -1
	for i, session := range sessions {
		if session.NodeID != nodeID || session.TargetID != targetID || inputFinished(session) {
			continue
		}
		if index >= 0 {
			return -1, fmt.Errorf("%s: session_id is required when multiple sessions are pending", inputToolName)
		}
		index = i
	}
	if index < 0 {
		return -1, fmt.Errorf("%s: no pending session", inputToolName)
	}
	return index, nil
}

func inputSource(session *InputProgress, sourceID string) (*InputSourceProgress, error) {
	for i := range session.Sources {
		if session.Sources[i].ID == sourceID {
			return &session.Sources[i], nil
		}
	}
	return nil, fmt.Errorf("%s: unknown source %q", inputToolName, sourceID)
}

func inputSourceStatus(source InputSourceProgress) string {
	switch {
	case source.Applied && (!source.AppliedAll || source.Discarded):
		return "partially_applied"
	case source.Applied:
		return "applied"
	case source.Discarded:
		return "discarded"
	default:
		return "unreviewed"
	}
}

func inputChangesCovered(changes []vfs.InputChange, applied []string) bool {
	if len(changes) != len(applied) {
		return false
	}
	for i, change := range changes {
		if change.Path != applied[i] {
			return false
		}
	}
	return true
}

func hasInputChange(changes []vfs.InputChange, path string) bool {
	for _, change := range changes {
		if change.Path == path {
			return true
		}
	}
	return false
}

func appendUnique(existing []string, items ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(items))
	for _, item := range existing {
		seen[item] = struct{}{}
	}
	for _, item := range items {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		existing = append(existing, item)
	}
	slices.Sort(existing)
	return existing
}

func readInputFile(store *vfs.Store, envID, path string, offset, limit int) any {
	data, err := store.View(envID).Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{"exists": false}
	}
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	content, next := pageText(string(data), offset, limit)
	return map[string]any{"exists": true, "content": content, "next_offset": next}
}

func pageText(value string, offset, limit int) (string, *int) {
	runes := []rune(value)
	if offset > len(runes) {
		offset = len(runes)
	}
	if limit <= 0 {
		limit = 4000
	}
	end := min(len(runes), offset+limit)
	if end == len(runes) {
		return string(runes[offset:end]), nil
	}
	return string(runes[offset:end]), &end
}

func pageChanges(changes []vfs.InputChange, offset, limit int) ([]vfs.InputChange, *int) {
	start, end, next := pageRange(len(changes), offset, limit, 200)
	return changes[start:end], next
}

func pageRange(length, offset, limit, defaultLimit int) (int, int, *int) {
	if offset > length {
		offset = length
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	end := min(length, offset+limit)
	if end == length {
		return offset, end, nil
	}
	return offset, end, &end
}

func cloneInputProgresses(sessions []InputProgress) []InputProgress {
	out := make([]InputProgress, len(sessions))
	for i, session := range sessions {
		out[i] = session
		out[i].Paths = slices.Clone(session.Paths)
		out[i].Sources = make([]InputSourceProgress, len(session.Sources))
		for k, source := range session.Sources {
			out[i].Sources[k] = source
			out[i].Sources[k].AppliedPaths = slices.Clone(source.AppliedPaths)
		}
	}
	return out
}

func preview(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func inputNotice(session InputProgress) string {
	sources := make([]string, 0, len(session.Sources))
	for _, source := range session.Sources {
		sources = append(sources, source.ID)
	}
	return fmt.Sprintf(
		"[input pending] session_id=%s sources=%s。共同状态已直接使用；请用 input list/inspect/apply/discard 处理差异，再调用 input finish 结束文件阶段。",
		session.ID,
		strings.Join(sources, ","),
	)
}
