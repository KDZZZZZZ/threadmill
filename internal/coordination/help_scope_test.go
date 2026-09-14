package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestAssembledPlannerHelpRetainsAcceptedFilesWithoutExperimentalEdits(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	ctx := helpContext(t, graph)
	files := vfs.NewStore(t.TempDir())
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	notified := make(chan string, 1)
	tools := graph.HelpTools(func(message string) { notified <- message })
	executorRead := make(chan agenttool.Result, 1)
	var resolved, organized atomic.Bool

	call := func(id, name string, args any) (agent.AssistantMessage, error) {
		data, err := json.Marshal(args)
		return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
			ID: id, Name: name, Arguments: data,
		}}}, err
	}
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		for _, message := range request.Messages {
			if result := message.ToolResult; result != nil && result.IsError && result.CallID != "read-feature" {
				return agent.AssistantMessage{}, fmt.Errorf("%s: %s", result.CallID, result.Content)
			}
		}
		switch {
		case hasTool(request.Tools, "input"):
			resolved.Store(true)
			listed := helpScopeToolResult(request, "list-input")
			if listed == nil {
				return call("list-input", "input", map[string]string{"action": "list"})
			}
			var list struct {
				Sessions []struct {
					ID      string                `json:"session_id"`
					Sources []struct{ ID string } `json:"sources"`
				} `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(listed.Content), &list); err != nil {
				return agent.AssistantMessage{}, err
			}
			if len(list.Sessions) != 1 {
				return agent.AssistantMessage{}, fmt.Errorf("input sessions = %d, want 1", len(list.Sessions))
			}
			session := list.Sessions[0]
			var accepted string
			var discarded []string
			for _, source := range session.Sources {
				if source.ID == "support:1:verifier" {
					accepted = source.ID
				} else {
					discarded = append(discarded, source.ID)
				}
			}
			if accepted == "" {
				return agent.AssistantMessage{}, errors.New("helper output is missing from the input batch")
			}
			for _, step := range []struct {
				id   string
				args map[string]any
			}{
				{"inspect-input", map[string]any{"action": "inspect", "source_id": accepted, "view": "diff"}},
				{"apply-input", map[string]any{"action": "apply", "source_id": accepted, "all": true, "strategy": "safe"}},
				{"discard-input", map[string]any{"action": "discard", "source_ids": discarded, "reason": "retain the accepted helper version"}},
				{"finish-input", map[string]any{"action": "finish", "reason": "helper files selected"}},
			} {
				if helpScopeToolResult(request, step.id) == nil {
					step.args["session_id"] = session.ID
					return call(step.id, "input", step.args)
				}
			}
			return agent.AssistantMessage{Content: "file input finished"}, nil
		case hasTool(request.Tools, "memory_apply"):
			if !strings.Contains(firstUserContent(request.Messages), "feature.go") {
				return agent.AssistantMessage{}, errors.New("organizer did not receive final helper file evidence")
			}
			organized.Store(true)
			return agent.AssistantMessage{Content: "memory differences retained with their source qualifications"}, nil
		case request.SystemPrompt == task.Planner.ID:
			if helpScopeToolResult(request, "before-help") == nil {
				return call("before-help", "write", map[string]string{"path": "planner-before.txt", "content": "private probe"})
			}
			if helpScopeToolResult(request, "help-call") == nil {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{helpCall()}}, nil
			}
			if helpScopeToolResult(request, "after-help") == nil {
				return call("after-help", "write", map[string]string{"path": "planner-after.txt", "content": "private experiment"})
			}
			return agent.AssistantMessage{Content: "use the accepted helper file"}, nil
		case request.SystemPrompt == "support:1:executor":
			if helpScopeToolResult(request, "helper-file") == nil {
				return call("helper-file", "write", map[string]string{"path": "feature.go", "content": "package feature\n"})
			}
			return agent.AssistantMessage{Content: "helper file ready"}, nil
		case request.SystemPrompt == task.Executor.ID:
			if result := helpScopeToolResult(request, "read-feature"); result != nil {
				executorRead <- *result
				return agent.AssistantMessage{Content: "executor inspected its inherited file"}, nil
			}
			return call("read-feature", "read", map[string]string{"path": "feature.go"})
		case request.SystemPrompt == task.Verifier.ID:
			if helpScopeToolResult(request, "verifier-probe") == nil {
				return call("verifier-probe", "write", map[string]string{"path": "verifier.txt", "content": "private verification"})
			}
		}
		return agent.AssistantMessage{Content: "role complete"}, nil
	})
	assemble := func(current Task) (Roles, error) {
		agents := agent.FileAgents{
			Planner:  agent.FileAgent{SystemPrompt: current.Planner.ID, Tools: []string{"write", "coordination_requestHelp"}},
			Executor: agent.FileAgent{SystemPrompt: current.Executor.ID, Tools: []string{"read", "write"}},
			Verifier: agent.FileAgent{SystemPrompt: current.Verifier.ID, Tools: []string{"write"}},
		}
		return Assemble(stores, provider, agents, nil, 0, nil, agent.FileOverlay{NamedTools: tools})(current)
	}
	done := runHelpTask(ctx, graph, task, stores, assemble)
	message := awaitHelpMessage(t, ctx, notified)
	pauseID, resumeID := helpNotificationEndpoints(t, message)
	if _, err := provideHelpTasks(t, graph, message, PendingSubgraph{
		Tasks: []PendingTask{{ID: "support", Info: "produce a feature fixture"}},
		Edges: []Edge{{From: pauseID, To: "support:1:planner"}, {From: "support:1:verifier", To: resumeID}},
	}); err != nil {
		t.Fatal(err)
	}
	awaitHelpDone(t, ctx, done)
	if !resolved.Load() || !organized.Load() {
		t.Fatalf("accepted input skipped a stage: files=%v memory=%v", resolved.Load(), organized.Load())
	}
	select {
	case result := <-executorRead:
		if result.IsError || !strings.Contains(result.Content, "package feature") {
			t.Errorf("downstream Executor lost the accepted helper file: %+v", result)
		}
	default:
		t.Error("downstream Executor did not inspect its inherited file")
	}
	for _, nodeID := range []string{pauseID, resumeID, task.Planner.ID, task.Executor.ID, task.Verifier.ID} {
		output, ok := graph.Output(nodeID)
		if !ok {
			t.Fatalf("missing output %s", nodeID)
		}
		if nodeID != pauseID {
			body, err := files.View(output.FilesRef).Read("feature.go")
			if err != nil || string(body) != "package feature\n" {
				t.Errorf("%s lost accepted helper files: %q, %v", nodeID, body, err)
			}
		}
		for _, path := range []string{"planner-before.txt", "planner-after.txt", "verifier.txt"} {
			if _, err := files.View(output.FilesRef).Read(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s exported private role edit %s: %v", nodeID, path, err)
			}
		}
	}
}

func helpScopeToolResult(request agent.Request, callID string) *agenttool.Result {
	for _, message := range request.Messages {
		if result := message.ToolResult; result != nil && result.CallID == callID {
			return result
		}
	}
	return nil
}

func TestRetriedDisposableObservationReferencesItsActualFiles(t *testing.T) {
	graph := New()
	task := graph.AddTask()
	failure := errors.New("export journal unavailable")
	graph.SetProgressStore(&observationRetryProgress{ProgressStore: &memoryProgressStore{}, failure: failure})
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}
	var attempt int
	assemble := func(task Task) (Roles, error) {
		finish := askerFunc(func(context.Context, string) (string, error) { return "done", nil })
		return Roles{
			Planner: askerFunc(func(context.Context, string) (string, error) {
				attempt++
				observation := fmt.Sprintf("experiment %d", attempt)
				if err := stores.Files.View(task.Env.ID+":planner").Write("probe.txt", []byte(observation)); err != nil {
					return "", err
				}
				err := stores.Memory.AppendNode(task.Env.ID, ctxgraph.Subgraph{ID: "evidence"}, ctxgraph.Node{
					ID: "observation", Kind: ctxgraph.NodeKindFact, Statement: observation, Status: ctxgraph.NodeStatusAccepted,
				})
				return observation, err
			}),
			Executor: finish, Verifier: finish,
			bind: func(string, string, string) error { return nil },
		}, nil
	}
	if _, err := graph.Run(context.Background(), task.ID, "observe", stores, assemble); !errors.Is(err, failure) {
		t.Fatalf("first export = %v, want journal failure", err)
	}
	if _, err := graph.Run(context.Background(), task.ID, "retry", stores, assemble); err != nil {
		t.Fatal(err)
	}
	output, ok := graph.Output(task.Planner.ID)
	if !ok {
		t.Fatal("retried planner did not commit its output")
	}
	memory, _ := stores.Memory.Snapshot(output.MemoryRef)
	for _, node := range memory.Nodes {
		if node.ID != "observation" {
			continue
		}
		if node.Status != ctxgraph.NodeStatusDisputed || len(node.SourceRefs) != 1 {
			t.Fatalf("private experiment must remain scoped: %+v", node)
		}
		content, err := stores.Files.View(node.SourceRefs[0]).Read("probe.txt")
		if err != nil || string(content) != node.Statement {
			t.Fatalf("retried observation %q refers to different files %q: %v", node.Statement, content, err)
		}
		return
	}
	t.Fatal("retried planner lost its observation")
}

type observationRetryProgress struct {
	ProgressStore
	failure error
	failed  bool
}

func (s *observationRetryProgress) Save(id string, progress TaskProgress) error {
	if progress.Pending != nil && !s.failed {
		s.failed = true
		return s.failure
	}
	return s.ProgressStore.Save(id, progress)
}
