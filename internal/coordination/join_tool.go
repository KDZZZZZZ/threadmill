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

const joinToolName = "join"

type joinCoordinator struct {
	graph *Graph

	mu       sync.Mutex
	runner   *runner
	sessions map[string][]JoinProgress
}

type joinTool struct{ join *joinCoordinator }

func (t joinTool) Definition() agenttool.Definition {
	return agenttool.Definition{
		Name:        joinToolName,
		Description: "检查并处理 join 候选。候选不会自动改动当前工作区；可查看输出/改动/文件，按路径安全采纳或显式覆盖，也可丢弃。完成当前角色前必须逐个处理候选并 finish。",
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

func (t joinTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	if err := ctx.Err(); err != nil {
		return agenttool.Output{}, err
	}
	if t.join == nil {
		return agenttool.Output{}, fmt.Errorf("%s: unavailable", joinToolName)
	}
	var args joinArgs
	if err := decodeGraphArgs(call.Arguments, &args); err != nil {
		return agenttool.Output{}, err
	}
	if args.Offset < 0 || args.Limit < 0 || args.Limit > 1000 {
		return agenttool.Output{}, fmt.Errorf("%s: invalid pagination", joinToolName)
	}
	result, err := t.join.execute(
		agenttool.AgentID(ctx),
		agenttool.EnvFromContext(ctx),
		args,
	)
	if err != nil {
		return agenttool.Output{}, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return agenttool.Output{}, fmt.Errorf("%s: encode result: %w", joinToolName, err)
	}
	return agenttool.Output{Content: string(data), Details: data}, nil
}

type joinArgs struct {
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

func (j *joinCoordinator) bind(r *runner) {
	j.mu.Lock()
	j.runner = r
	j.mu.Unlock()
}

func (j *joinCoordinator) forget(taskIDs []string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, taskID := range taskIDs {
		delete(j.sessions, taskID)
	}
}

func (j *joinCoordinator) open(
	taskID, nodeID, targetID, sessionID string,
	items []joinedTask,
) (JoinProgress, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	sessions, err := j.loadLocked(taskID)
	if err != nil {
		return JoinProgress{}, err
	}
	for _, session := range sessions {
		if session.ID == sessionID {
			return session, nil
		}
	}
	session := JoinProgress{
		ID:       sessionID,
		NodeID:   nodeID,
		TargetID: targetID,
		Sources:  make([]JoinSourceProgress, 0, len(items)),
	}
	for _, item := range items {
		session.Sources = append(session.Sources, JoinSourceProgress{
			TaskID: item.task.ID,
			EnvID:  item.task.Env.ID,
			Output: item.out,
		})
	}
	sessions = append(sessions, session)
	if err := j.saveLocked(taskID, sessions); err != nil {
		return JoinProgress{}, err
	}
	return session, nil
}

func (j *joinCoordinator) execute(nodeID, targetID string, args joinArgs) (any, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("%s: caller agent is unknown", joinToolName)
	}
	taskID, ok := j.taskID(nodeID)
	if !ok {
		return nil, fmt.Errorf("%s: caller node %q is unknown", joinToolName, nodeID)
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
	index, err := selectJoinSession(sessions, nodeID, targetID, strings.TrimSpace(args.SessionID))
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, fmt.Errorf("%s: unknown session %q", joinToolName, args.SessionID)
	}
	session := &sessions[index]
	if session.NodeID != nodeID || session.TargetID != targetID {
		return nil, fmt.Errorf("%s: session %q does not belong to this role workspace", joinToolName, session.ID)
	}
	if session.Finished {
		if args.Action == "finish" {
			if err := j.releaseLocked(*session); err != nil {
				return nil, err
			}
			return map[string]any{"session_id": session.ID, "finished": true, "task_id": taskID}, nil
		}
		return nil, fmt.Errorf("%s: session %q is already finished", joinToolName, session.ID)
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
		err = fmt.Errorf("%s: unknown action %q", joinToolName, args.Action)
	}
	if err != nil {
		return nil, err
	}
	if args.Action != "inspect" {
		if err := j.saveLocked(taskID, sessions); err != nil {
			return nil, err
		}
	}
	if args.Action == "finish" {
		if err := j.releaseLocked(*session); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (j *joinCoordinator) listLocked(
	sessions []JoinProgress,
	nodeID, targetID string,
	args joinArgs,
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
		if session.Finished {
			status = "finished"
		}
		start, end, next := pageRange(len(session.Sources), args.Offset, args.Limit, 100)
		item := summary{
			ID: session.ID, Status: status,
			Sources: make([]source, 0, end-start), NextOffset: next,
		}
		for _, candidate := range session.Sources[start:end] {
			item.Sources = append(item.Sources, source{
				ID:      candidate.TaskID,
				Status:  joinSourceStatus(candidate),
				Preview: preview(candidate.Output, 300),
			})
		}
		result.Sessions = append(result.Sessions, item)
	}
	return result
}

func (j *joinCoordinator) inspectLocked(session JoinProgress, args joinArgs) (any, error) {
	view := args.View
	if view == "" {
		view = "summary"
	}
	if view == "compare" {
		return j.compareLocked(session, args)
	}
	source, err := joinSource(&session, strings.TrimSpace(args.SourceID))
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
			"source_id":      source.TaskID,
			"status":         joinSourceStatus(*source),
			"output_preview": preview(source.Output, 1000),
			"changed_paths":  len(changes),
		}, nil
	case "output":
		output, next := pageText(source.Output, args.Offset, args.Limit)
		return map[string]any{"source_id": source.TaskID, "output": output, "next_offset": next}, nil
	case "diff":
		page, next := pageChanges(changes, args.Offset, args.Limit)
		return map[string]any{"source_id": source.TaskID, "changes": page, "next_offset": next}, nil
	case "file":
		path := strings.TrimSpace(args.Path)
		if path == "" {
			return nil, fmt.Errorf("%s: path is required for file inspection", joinToolName)
		}
		if !hasJoinChange(changes, path) {
			return nil, fmt.Errorf("%s: path %q is not a candidate change", joinToolName, path)
		}
		if j.runner == nil || j.runner.stores.Files == nil {
			return nil, fmt.Errorf("%s: file store is unavailable", joinToolName)
		}
		data, readErr := j.runner.stores.Files.View(source.EnvID).Read(path)
		if errors.Is(readErr, fs.ErrNotExist) {
			return map[string]any{"source_id": source.TaskID, "path": path, "deleted": true}, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		content, next := pageText(string(data), args.Offset, args.Limit)
		return map[string]any{
			"source_id":   source.TaskID,
			"path":        path,
			"content":     content,
			"next_offset": next,
		}, nil
	default:
		return nil, fmt.Errorf("%s: unknown inspect view %q", joinToolName, view)
	}
}

func (j *joinCoordinator) compareLocked(session JoinProgress, args joinArgs) (any, error) {
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return nil, fmt.Errorf("%s: path is required for compare", joinToolName)
	}
	if j.runner == nil || j.runner.stores.Files == nil {
		return nil, fmt.Errorf("%s: file store is unavailable", joinToolName)
	}
	changed := false
	for _, source := range session.Sources {
		changes, err := j.changesLocked(source.EnvID)
		if err != nil {
			return nil, err
		}
		changed = changed || hasJoinChange(changes, path)
	}
	if !changed {
		return nil, fmt.Errorf("%s: path %q is not changed by this session", joinToolName, path)
	}
	result := map[string]any{
		"path":   path,
		"target": readJoinFile(j.runner.stores.Files, session.TargetID, path, args.Offset, args.Limit),
	}
	sources := make(map[string]any, len(session.Sources))
	for _, source := range session.Sources {
		sources[source.TaskID] = readJoinFile(
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

func (j *joinCoordinator) applyLocked(session *JoinProgress, args joinArgs) (any, error) {
	source, err := joinSource(session, strings.TrimSpace(args.SourceID))
	if err != nil {
		return nil, err
	}
	strategy := args.Strategy
	if strategy == "" {
		strategy = "safe"
	}
	if strategy != "safe" && strategy != "replace" {
		return nil, fmt.Errorf("%s: unknown apply strategy %q", joinToolName, strategy)
	}
	if args.All == (len(args.Paths) > 0) {
		return nil, fmt.Errorf("%s: apply requires exactly one of paths or all=true", joinToolName)
	}
	if strategy == "replace" && strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: replace requires reason", joinToolName)
	}
	paths := args.Paths
	if args.All {
		paths = nil
	}
	var result vfs.JoinApplyResult
	if j.runner != nil && j.runner.stores.Files != nil {
		result, err = j.runner.stores.Files.ApplyJoin(
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
				"source_id": source.TaskID,
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
	source.AppliedAll = joinChangesCovered(changes, source.AppliedPaths)
	return map[string]any{
		"source_id": source.TaskID,
		"strategy":  strategy,
		"applied":   result.Applied,
		"status":    joinSourceStatus(*source),
	}, nil
}

func (j *joinCoordinator) discardLocked(session *JoinProgress, args joinArgs) (any, error) {
	if len(args.SourceIDs) == 0 && strings.TrimSpace(args.SourceID) != "" {
		args.SourceIDs = []string{args.SourceID}
	}
	if len(args.SourceIDs) == 0 {
		return nil, fmt.Errorf("%s: source_ids is required", joinToolName)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: discard requires reason", joinToolName)
	}
	for _, sourceID := range args.SourceIDs {
		source, err := joinSource(session, strings.TrimSpace(sourceID))
		if err != nil {
			return nil, err
		}
		source.Discarded = true
		source.Reason = strings.TrimSpace(args.Reason)
	}
	return map[string]any{"discarded": args.SourceIDs}, nil
}

func (j *joinCoordinator) finishLocked(taskID string, session *JoinProgress, args joinArgs) (any, error) {
	if strings.TrimSpace(args.Reason) == "" {
		return nil, fmt.Errorf("%s: finish requires reason", joinToolName)
	}
	undecided := make([]string, 0)
	for _, source := range session.Sources {
		if !source.AppliedAll && !source.Discarded {
			undecided = append(undecided, source.TaskID)
		}
	}
	if len(undecided) > 0 {
		return nil, fmt.Errorf("%s: undecided sources: %s", joinToolName, strings.Join(undecided, ", "))
	}
	session.Finished = true
	session.Reason = strings.TrimSpace(args.Reason)
	return map[string]any{"session_id": session.ID, "finished": true, "task_id": taskID}, nil
}

func (j *joinCoordinator) releaseLocked(session JoinProgress) error {
	if j.runner == nil {
		return nil
	}
	var releaseErr error
	for _, source := range session.Sources {
		if err := j.runner.stores.DiscardFiles(source.EnvID); err != nil {
			releaseErr = errors.Join(
				releaseErr,
				fmt.Errorf("%s: release candidate %s: %w", joinToolName, source.TaskID, err),
			)
		}
	}
	return releaseErr
}

func (j *joinCoordinator) requireFinished(nodeID string) error {
	taskID, ok := j.taskID(nodeID)
	if !ok {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	sessions, err := j.loadLocked(taskID)
	if err != nil {
		return err
	}
	var pending []string
	for _, session := range sessions {
		if session.NodeID == nodeID && !session.Finished {
			pending = append(pending, session.ID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("coordination: role %s returned with unfinished join sessions: %s", nodeID, strings.Join(pending, ", "))
	}
	return nil
}

func (j *joinCoordinator) loadLocked(taskID string) ([]JoinProgress, error) {
	if sessions, ok := j.sessions[taskID]; ok {
		return cloneJoinProgresses(sessions), nil
	}
	var sessions []JoinProgress
	if j.runner != nil && j.runner.progress != nil {
		progress, ok, err := j.runner.progress.Load(taskID)
		if err != nil {
			return nil, fmt.Errorf("loading join progress: %w", err)
		}
		if ok {
			sessions = progress.Joins
		}
	}
	j.sessions[taskID] = cloneJoinProgresses(sessions)
	return cloneJoinProgresses(sessions), nil
}

func (j *joinCoordinator) saveLocked(taskID string, sessions []JoinProgress) error {
	cloned := cloneJoinProgresses(sessions)
	if j.runner != nil && j.runner.progress != nil {
		progress, _, err := j.runner.progress.Load(taskID)
		if err != nil {
			return fmt.Errorf("loading join progress: %w", err)
		}
		progress.Joins = cloned
		for _, session := range sessions {
			if session.Finished && strings.HasPrefix(session.ID, "join:incoming:") && !hasProgressID(progress.Merged, session.NodeID) {
				progress.Merged = append(progress.Merged, session.NodeID)
			}
		}
		if err := j.runner.progress.Save(taskID, progress); err != nil {
			return fmt.Errorf("saving join progress: %w", err)
		}
	}
	j.sessions[taskID] = cloned
	return nil
}

func (j *joinCoordinator) changesLocked(envID string) ([]vfs.JoinChange, error) {
	if j.runner == nil || j.runner.stores.Files == nil {
		return []vfs.JoinChange{}, nil
	}
	return j.runner.stores.Files.JoinChanges(envID)
}

func (j *joinCoordinator) taskID(nodeID string) (string, bool) {
	if j.graph == nil {
		return "", false
	}
	j.graph.mu.Lock()
	defer j.graph.mu.Unlock()
	node, ok := j.graph.nodeByIDLocked(nodeID)
	return node.TaskID, ok
}

func selectJoinSession(
	sessions []JoinProgress,
	nodeID, targetID, sessionID string,
) (int, error) {
	if sessionID != "" {
		return slices.IndexFunc(sessions, func(session JoinProgress) bool {
			return session.ID == sessionID
		}), nil
	}
	index := -1
	for i, session := range sessions {
		if session.NodeID != nodeID || session.TargetID != targetID || session.Finished {
			continue
		}
		if index >= 0 {
			return -1, fmt.Errorf("%s: session_id is required when multiple sessions are pending", joinToolName)
		}
		index = i
	}
	if index < 0 {
		return -1, fmt.Errorf("%s: no pending session", joinToolName)
	}
	return index, nil
}

func joinSource(session *JoinProgress, sourceID string) (*JoinSourceProgress, error) {
	for i := range session.Sources {
		if session.Sources[i].TaskID == sourceID {
			return &session.Sources[i], nil
		}
	}
	return nil, fmt.Errorf("%s: unknown source %q", joinToolName, sourceID)
}

func joinSourceStatus(source JoinSourceProgress) string {
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

func joinChangesCovered(changes []vfs.JoinChange, applied []string) bool {
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

func hasJoinChange(changes []vfs.JoinChange, path string) bool {
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

func readJoinFile(store *vfs.Store, envID, path string, offset, limit int) any {
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

func pageChanges(changes []vfs.JoinChange, offset, limit int) ([]vfs.JoinChange, *int) {
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

func cloneJoinProgresses(sessions []JoinProgress) []JoinProgress {
	out := make([]JoinProgress, len(sessions))
	for i, session := range sessions {
		out[i] = session
		out[i].Sources = make([]JoinSourceProgress, len(session.Sources))
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

func joinNotice(session JoinProgress) string {
	sources := make([]string, 0, len(session.Sources))
	for _, source := range session.Sources {
		sources = append(sources, source.TaskID)
	}
	return fmt.Sprintf(
		"[join pending] session_id=%s sources=%s。候选尚未修改当前工作区；请用 join list/inspect/apply/discard 处理，并在结束本角色前调用 join finish。",
		session.ID,
		strings.Join(sources, ","),
	)
}
