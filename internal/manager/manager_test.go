package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/agent"
	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/logging"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestOpenUsesOneUserStateDirectoryForCanonicalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "project-link")
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	provider := stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
		return agent.AssistantMessage{Content: "idle"}, nil
	})

	for _, root := range []string{project, alias} {
		mgr, err := Open(context.Background(), Options{
			Root:     root,
			File:     loadRepoConfig(t),
			Provider: provider,
		})
		if err != nil {
			t.Fatalf("Open(%q) error = %v", root, err)
		}
		mgr.Close()
	}

	projects, err := os.ReadDir(filepath.Join(home, ".threadmill", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || !projects[0].IsDir() {
		t.Fatalf("projects = %#v, want one project state directory", projects)
	}
	if _, err := os.Stat(filepath.Join(project, ".threadmill")); !os.IsNotExist(err) {
		t.Fatalf("project-local .threadmill exists or stat failed: %v", err)
	}
}

func TestStateUsesUserProjectDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()

	paths, err := openStatePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Dir(paths.LogFile)
	wantParent := filepath.Join(home, ".threadmill", "projects")
	if got := filepath.Dir(projectDir); got != wantParent {
		t.Fatalf("project state parent = %q, want %q", got, wantParent)
	}
	if paths.ManagerReactDir != filepath.Join(projectDir, "checkpoints", "manager") ||
		paths.ReactDir != filepath.Join(projectDir, "checkpoints", "tasks") ||
		paths.ProgressDir != filepath.Join(projectDir, "progress") ||
		paths.VFSDir != filepath.Join(projectDir, "vfs") {
		t.Fatalf("state paths = %#v, want checkpoints and progress under project state", paths)
	}
}

func TestOpenRestoresProjectGraph(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := t.TempDir()
	created := false
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		switch {
		case strings.Contains(request.SystemPrompt, "记忆整理器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(request.SystemPrompt, "经理 Agent"):
			if strings.Contains(lastUser(request.Messages), "[任务报告]") {
				return agent.AssistantMessage{Content: "done"}, nil
			}
			if !created {
				created = true
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "restore-graph",
					Name:      "coordination_replacePending",
					Arguments: json.RawMessage(`{"roots":[{"info":"persist me"}],"spawns":[]}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "started"}, nil
		default:
			return agent.AssistantMessage{Content: "role done"}, nil
		}
	})
	first, err := Open(ctx, Options{
		Root:     project,
		File:     loadRepoConfig(t),
		Provider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	first.Send("persist")
	if err := first.WaitIdle(ctx); err != nil {
		first.Close()
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(ctx, Options{
		Root: project,
		File: loadRepoConfig(t),
		Provider: stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "idle"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	got := second.Snapshot()
	if len(got.Tasks) != 1 || got.Tasks[0].Info != "persist me" || got.Tasks[0].Outcome != coordination.OutcomeDone {
		t.Fatalf("restored graph = %#v, want completed persist-me task", got)
	}
}

func TestOpenResumesActiveProjectTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := t.TempDir()
	paths, err := openStatePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := coordination.OpenGraph(paths.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ReplacePending(ctx, coordination.PendingSubgraph{
		Roots: []coordination.PendingRoot{{Info: "resume me"}},
	}); err != nil {
		t.Fatal(err)
	}

	mgr, err := Open(ctx, Options{
		Root: project,
		File: loadRepoConfig(t),
		Provider: stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
			switch {
			case strings.Contains(request.SystemPrompt, "记忆整理器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			case strings.Contains(request.SystemPrompt, "经理 Agent"):
				return agent.AssistantMessage{Content: "resumed"}, nil
			default:
				return agent.AssistantMessage{Content: "role done"}, nil
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}

	got := mgr.Snapshot()
	if len(got.Tasks) != 1 || got.Tasks[0].Info != "resume me" || got.Tasks[0].Outcome != coordination.OutcomeDone {
		t.Fatalf("resumed graph = %#v, want completed resume-me task", got)
	}
}

func TestOpenTracksResumedManagerTurnUntilIdle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := t.TempDir()
	paths, err := openStatePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	store, err := agent.NewDirCheckpointStore(paths.ManagerReactDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("manager", agent.Checkpoint{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "resume manager"}},
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	mgr, err := Open(ctx, Options{
		Root: project,
		File: loadRepoConfig(t),
		Provider: stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "经理 Agent") {
				startedOnce.Do(func() { close(started) })
				select {
				case <-release:
					return agent.AssistantMessage{Content: "resumed"}, nil
				case <-callCtx.Done():
					return agent.AssistantMessage{}, callCtx.Err()
				}
			}
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		mgr.Close()
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopWait()
	if err := mgr.WaitIdle(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdle() while manager is restoring = %v, want deadline exceeded", err)
	}

	releaseOnce.Do(func() { close(release) })
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFinishesResumedManagerTurnBeforeStartingActiveTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := t.TempDir()
	paths, err := openStatePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := coordination.OpenGraph(paths.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ReplacePending(ctx, coordination.PendingSubgraph{
		Roots: []coordination.PendingRoot{{Info: "start after manager recovery"}},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := agent.NewDirCheckpointStore(paths.ManagerReactDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("manager", agent.Checkpoint{
		Messages: []agent.Message{{Role: agent.RoleUser, Content: "finish manager recovery first"}},
	}); err != nil {
		t.Fatal(err)
	}

	managerStarted := make(chan struct{})
	releaseManager := make(chan struct{})
	taskStarted := make(chan struct{})
	var managerOnce, taskOnce sync.Once
	mgr, err := Open(ctx, Options{
		Root: project,
		File: loadRepoConfig(t),
		Provider: stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			switch {
			case strings.Contains(request.SystemPrompt, "经理 Agent"):
				managerOnce.Do(func() { close(managerStarted) })
				select {
				case <-releaseManager:
					return agent.AssistantMessage{Content: "manager recovered"}, nil
				case <-callCtx.Done():
					return agent.AssistantMessage{}, callCtx.Err()
				}
			case strings.Contains(request.SystemPrompt, "记忆整理器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			default:
				taskOnce.Do(func() { close(taskStarted) })
				return agent.AssistantMessage{Content: "role done"}, nil
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	select {
	case <-managerStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-taskStarted:
		t.Fatal("active task started before the manager checkpoint finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseManager)
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-taskStarted:
	default:
		t.Fatal("active task did not start after manager recovery")
	}
}

func TestOpenRestoresDurableTaskFilesBeforeResumingVerifier(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "work.txt"), []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := openStatePaths(project)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := coordination.OpenGraph(paths.GraphFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.ReplacePending(ctx, coordination.PendingSubgraph{
		Roots: []coordination.PendingRoot{{Info: "resume verifier with task files"}},
	}); err != nil {
		t.Fatal(err)
	}
	task := graph.Snapshot().Tasks[0]
	progress, err := coordination.NewDirProgressStore(paths.ProgressDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := progress.Save(task.ID, coordination.TaskProgress{
		Prepared: true,
		Outputs: map[string]string{
			task.Planner.ID:  "planned",
			task.Executor.ID: "executor changed work.txt",
		},
	}); err != nil {
		t.Fatal(err)
	}
	durableFiles, err := vfs.NewPersistentStore(project, paths.VFSDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := durableFiles.Fork("", task.Env.ID); err != nil {
		t.Fatal(err)
	}
	if err := durableFiles.View(task.Env.ID).Write("work.txt", []byte("task")); err != nil {
		t.Fatal(err)
	}
	if err := durableFiles.Release(task.Env.ID); err != nil {
		t.Fatal(err)
	}

	var verifierRead string
	mgr, err := Open(ctx, Options{
		Root: project,
		File: loadRepoConfig(t),
		Provider: stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
			switch {
			case strings.Contains(request.SystemPrompt, "经理 Agent"):
				return agent.AssistantMessage{Content: "reported"}, nil
			case strings.Contains(request.SystemPrompt, "记忆整理器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			case strings.Contains(request.SystemPrompt, "核验 Agent"):
				for _, message := range request.Messages {
					if message.Role == agent.RoleTool {
						verifierRead = message.Content
					}
				}
				if verifierRead != "" {
					return agent.AssistantMessage{Content: "结论: PASS"}, nil
				}
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "read-work",
					Name:      "read",
					Arguments: json.RawMessage(`{"path":"work.txt"}`),
				}}}, nil
			default:
				t.Fatalf("unexpected resumed role: %s", request.SystemPrompt)
				return agent.AssistantMessage{}, nil
			}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verifierRead, "task") {
		t.Fatalf("verifier read = %q, want durable task contents", verifierRead)
	}
}

func TestManagerRunsRootTaskAndWakesWithReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var managerQueries []string
	var replies []string
	var managerReportMemory string
	var managerAfterGraphMemory string
	var rootExecutorMemory string
	leakedManagerMemory := false
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		switch {
		case strings.Contains(sys, "记忆整理器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "经理 Agent"):
			mu.Lock()
			query := lastUser(request.Messages)
			managerQueries = append(managerQueries, query)
			if strings.Contains(query, "[任务报告]") {
				managerReportMemory = sys
			} else if hasToolResult(request.Messages) {
				managerAfterGraphMemory = sys
			}
			n := 0
			for _, q := range managerQueries {
				if !strings.Contains(q, "[任务报告]") {
					n++
				}
			}
			userTurns := n
			mu.Unlock()
			if strings.Contains(query, "[任务报告]") {
				return agent.AssistantMessage{Content: "任务已完成"}, nil
			}
			if userTurns == 1 && !hasToolResult(request.Messages) {
				args, err := json.Marshal(coordination.PendingSubgraph{
					Roots: []coordination.PendingRoot{{Info: "TASK INFO"}},
					Spawns: []coordination.PendingSpawn{{
						From: "task-1:planner",
						Join: "task-1:executor",
						Info: "CHILD INFO",
					}},
				})
				if err != nil {
					return agent.AssistantMessage{}, err
				}
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "r1",
					Name:      "coordination_replacePending",
					Arguments: args,
				}}}, nil
			}
			return agent.AssistantMessage{Content: "已建任务"}, nil
		case strings.Contains(sys, "规划 Agent"):
			mu.Lock()
			leakedManagerMemory = leakedManagerMemory || strings.Contains(sys, "USER MESSAGE")
			mu.Unlock()
			if strings.Contains(lastUser(request.Messages), "CHILD INFO") {
				return agent.AssistantMessage{Content: "child plan"}, nil
			}
			return agent.AssistantMessage{Content: "root plan"}, nil
		case strings.Contains(sys, "执行 Agent"):
			mu.Lock()
			leakedManagerMemory = leakedManagerMemory || strings.Contains(sys, "USER MESSAGE")
			if strings.Contains(lastUser(request.Messages), "[join]") {
				rootExecutorMemory = sys
			}
			mu.Unlock()
			if strings.Contains(lastUser(request.Messages), "child plan") {
				return agent.AssistantMessage{Content: "child did"}, nil
			}
			return agent.AssistantMessage{Content: "root did"}, nil
		default:
			mu.Lock()
			leakedManagerMemory = leakedManagerMemory || strings.Contains(sys, "USER MESSAGE")
			mu.Unlock()
			if strings.Contains(lastUser(request.Messages), "child did") {
				return agent.AssistantMessage{Content: "CHILD REPORT"}, nil
			}
			return agent.AssistantMessage{Content: "TASK REPORT"}, nil
		}
	})

	mgr, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
		Output: func(text string) {
			mu.Lock()
			replies = append(replies, text)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer mgr.Close()

	mgr.Send("USER MESSAGE")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}

	snap := mgr.Snapshot()
	if len(snap.Tasks) != 2 {
		t.Fatalf("tasks = %d, want root and child", len(snap.Tasks))
	}
	if snap.Tasks[0].Outcome != coordination.OutcomeDone {
		t.Fatalf("outcome = %q, want done", snap.Tasks[0].Outcome)
	}
	if snap.Tasks[0].Info != "TASK INFO" {
		t.Fatalf("info = %q, want TASK INFO", snap.Tasks[0].Info)
	}

	mu.Lock()
	defer mu.Unlock()
	foundReport := false
	for _, q := range managerQueries {
		if strings.Contains(q, "[任务报告]") && strings.Contains(q, "task-1") && strings.Contains(q, "TASK REPORT") {
			foundReport = true
		}
	}
	if !foundReport {
		t.Fatalf("manager queries = %q, want a task report", managerQueries)
	}
	for _, want := range []string{"[User Message] USER MESSAGE", "[Task Info] task-1: TASK INFO", "[Task Info] task-2: CHILD INFO"} {
		if !strings.Contains(managerAfterGraphMemory, want) {
			t.Fatalf("manager memory after graph update = %q, want %q", managerAfterGraphMemory, want)
		}
	}
	for _, want := range []string{"USER MESSAGE", "TASK INFO", "CHILD INFO", "CHILD REPORT", "TASK REPORT"} {
		if !strings.Contains(managerReportMemory, want) {
			t.Fatalf("manager report memory = %q, want %q", managerReportMemory, want)
		}
	}
	if !strings.Contains(rootExecutorMemory, "CHILD REPORT") {
		t.Fatalf("root executor memory = %q, want joined child report", rootExecutorMemory)
	}
	if leakedManagerMemory {
		t.Fatal("manager-only memory leaked into task role system prompt")
	}
	joined := strings.Join(replies, "\n")
	if !strings.Contains(joined, "已建任务") || !strings.Contains(joined, "任务已完成") {
		t.Fatalf("replies = %q, want manager status then completion", replies)
	}
}

func TestManagerBuildsMinimalTaskPackageFromTaskInfo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created := false
	var organizerQuery string
	var plannerMemory string
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "记忆整理器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "记忆子图整理 Agent"):
			organizerQuery = query
			if !hasToolResult(request.Messages) {
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "select-needed",
					Name:      "memory_add_to_subgraph",
					Arguments: json.RawMessage(`{"subgraph_id":"task-1-package","node_ids":["needed"]}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "organized"}, nil
		case strings.Contains(sys, "经理 Agent"):
			if strings.Contains(query, "[任务报告]") {
				return agent.AssistantMessage{Content: "done"}, nil
			}
			if !created {
				created = true
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "create-root",
					Name:      "coordination_replacePending",
					Arguments: json.RawMessage(`{"roots":[{"info":"use NEED_TOKEN"}],"spawns":[]}`),
				}}}, nil
			}
			return agent.AssistantMessage{Content: "started"}, nil
		case strings.Contains(sys, "规划 Agent"):
			plannerMemory = sys
			return agent.AssistantMessage{Content: "plan"}, nil
		case strings.Contains(sys, "执行 Agent"):
			return agent.AssistantMessage{Content: "executed"}, nil
		default:
			return agent.AssistantMessage{Content: "verified"}, nil
		}
	})

	mgr, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	graph := mgr.stores.Memory.Load(coordination.ManagerEnvID)
	graph = graph.WithSubgraph(ctxgraph.Subgraph{
		ID: "bloated", Name: "mixed context", Kind: ctxgraph.SubgraphKindGeneral,
	})
	graph = graph.WithMemory([]ctxgraph.Node{
		{ID: "needed", Kind: ctxgraph.NodeKindFact, Statement: "NEEDED FACT NEED_TOKEN", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"bloated"}},
		{ID: "noise", Kind: ctxgraph.NodeKindFact, Statement: "NOISE FACT", Status: ctxgraph.NodeStatusAccepted, SubgraphIDs: []string{"bloated"}},
	}, nil)
	mgr.stores.Memory.Save(coordination.ManagerEnvID, graph)

	mgr.Send("start")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(organizerQuery, "use NEED_TOKEN") || !strings.Contains(organizerQuery, "task-1-package") {
		t.Fatalf("organizer query = %q, want task info and package ID", organizerQuery)
	}
	if strings.Contains(organizerQuery, coordination.ManagerMemorySubgraphID) {
		t.Fatalf("organizer query = %q, contains manager-only subgraph", organizerQuery)
	}
	if !strings.Contains(plannerMemory, "NEEDED FACT NEED_TOKEN") {
		t.Fatalf("planner memory = %q, want selected fact", plannerMemory)
	}
	if strings.Contains(plannerMemory, "NOISE FACT") {
		t.Fatalf("planner memory = %q, contains unselected noise", plannerMemory)
	}
}

func TestManagerTaskRequestsHelpAndResumesAfterJoinedTask(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	created := false
	helpProvided := false
	helperFinished := 0
	resumedBeforeHelp := false
	var helpRequest string
	var requesterResult string
	var requesterMemory string
	var managerHelpMemory string
	var managerFinalMemory string
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "记忆整理器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "经理 Agent"):
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(query, "[任务报告]"):
				managerFinalMemory = sys
				return agent.AssistantMessage{Content: "任务已完成"}, nil
			case strings.Contains(query, "[拆分请求]"):
				helpRequest = query
				if !helpProvided {
					helpProvided = true
					requestID := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(query, "\n", 2)[0], "[拆分请求]"))
					args, err := json.Marshal(map[string]any{
						"request_id": requestID,
						"spawns": []map[string]string{
							{"from": "task-1:planner", "info": "gather evidence A"},
							{"from": "task-1:planner", "info": "gather evidence B"},
						},
					})
					if err != nil {
						return agent.AssistantMessage{}, err
					}
					return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
						ID:        "help-graph",
						Name:      "coordination_provideHelp",
						Arguments: args,
					}}}, nil
				}
				managerHelpMemory = sys
				return agent.AssistantMessage{Content: "正在拆分"}, nil
			case !created:
				created = true
				args, err := json.Marshal(coordination.PendingSubgraph{
					Roots: []coordination.PendingRoot{{Info: "solve"}},
				})
				if err != nil {
					return agent.AssistantMessage{}, err
				}
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "root-graph",
					Name:      "coordination_replacePending",
					Arguments: args,
				}}}, nil
			default:
				return agent.AssistantMessage{Content: "已开始"}, nil
			}
		case strings.Contains(sys, "规划 Agent"):
			if strings.Contains(query, "gather evidence A") {
				return agent.AssistantMessage{Content: "helper-plan-A"}, nil
			}
			if strings.Contains(query, "gather evidence B") {
				return agent.AssistantMessage{Content: "helper-plan-B"}, nil
			}
			return agent.AssistantMessage{Content: "root-plan"}, nil
		case strings.Contains(sys, "执行 Agent"):
			if strings.Contains(query, "helper-plan-A") {
				return agent.AssistantMessage{Content: "helper-did-A"}, nil
			}
			if strings.Contains(query, "helper-plan-B") {
				return agent.AssistantMessage{Content: "helper-did-B"}, nil
			}
			if result := lastToolResult(request.Messages); result != "" {
				mu.Lock()
				requesterResult = result
				requesterMemory = sys
				resumedBeforeHelp = helperFinished != 2
				mu.Unlock()
				return agent.AssistantMessage{Content: "root-did"}, nil
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "need-help",
				Name:      "coordination_requestHelp",
				Arguments: json.RawMessage(`{"reason":"need evidence"}`),
			}}}, nil
		default:
			if strings.Contains(query, "helper-did-A") {
				mu.Lock()
				helperFinished++
				mu.Unlock()
				return agent.AssistantMessage{Content: "helper-result-A"}, nil
			}
			if strings.Contains(query, "helper-did-B") {
				mu.Lock()
				helperFinished++
				mu.Unlock()
				return agent.AssistantMessage{Content: "helper-result-B"}, nil
			}
			return agent.AssistantMessage{Content: "root-verified"}, nil
		}
	})

	mgr, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer mgr.Close()

	mgr.Send("solve")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}

	snap := mgr.Snapshot()
	if len(snap.Tasks) != 3 {
		t.Fatalf("tasks = %d, want root and two helpers", len(snap.Tasks))
	}
	for _, task := range snap.Tasks {
		if task.Outcome != coordination.OutcomeDone {
			t.Fatalf("task %s outcome = %q, want done", task.ID, task.Outcome)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(helpRequest, "task-1:executor") || !strings.Contains(helpRequest, "need evidence") {
		t.Fatalf("help request = %q, want requester node and reason", helpRequest)
	}
	if resumedBeforeHelp {
		t.Fatal("requester resumed before helper verifier finished")
	}
	if !strings.Contains(requesterResult, "helper-result-A") || !strings.Contains(requesterResult, "helper-result-B") {
		t.Fatalf("requester tool result = %q, want both joined helper outputs", requesterResult)
	}
	if !strings.Contains(requesterMemory, "helper-result-A") || !strings.Contains(requesterMemory, "helper-result-B") {
		t.Fatalf("requester memory = %q, want both joined helper reports", requesterMemory)
	}
	for _, want := range []string{"gather evidence A", "gather evidence B"} {
		if !strings.Contains(managerHelpMemory, want) {
			t.Fatalf("manager help memory = %q, want %q", managerHelpMemory, want)
		}
	}
	for _, want := range []string{"helper-result-A", "helper-result-B"} {
		if !strings.Contains(managerFinalMemory, want) {
			t.Fatalf("manager final memory = %q, want %q", managerFinalMemory, want)
		}
	}
}

func TestManagerIdleWhenOnlyTalking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if strings.Contains(request.SystemPrompt, "记忆整理器") {
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return agent.AssistantMessage{Content: "hello"}, nil
	})
	mgr, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	mgr.Send("hi")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if n := len(mgr.Snapshot().Tasks); n != 0 {
		t.Fatalf("tasks = %d, want 0", n)
	}
}

func TestManagerLogsRuntimeEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var logs bytes.Buffer
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if strings.Contains(request.SystemPrompt, "记忆整理器") {
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		}
		return agent.AssistantMessage{Content: "hello"}, nil
	})
	mgr, err := Open(ctx, Options{
		Root:     t.TempDir(),
		File:     loadRepoConfig(t),
		Provider: provider,
		Logger:   logging.New(logging.Config{Output: &logs, JSON: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	mgr.Send("hi")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if !strings.Contains(logs.String(), `"msg":"runtime event"`) {
		t.Fatalf("logs = %s, want runtime event", logs.String())
	}
}

func loadRepoConfig(t *testing.T) provider.FileConfig {
	t.Helper()
	cfg, err := provider.LoadConfig(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type stubProvider func(context.Context, agent.Request) (agent.AssistantMessage, error)

func (f stubProvider) Generate(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
	return f(ctx, request)
}

func lastUser(messages []agent.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

func hasToolResult(messages []agent.Message) bool {
	for _, message := range messages {
		if message.Role == agent.RoleTool {
			return true
		}
	}
	return false
}

func lastToolResult(messages []agent.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == agent.RoleTool {
			return messages[i].Content
		}
	}
	return ""
}

func TestFormatReport(t *testing.T) {
	t.Parallel()

	got := formatReport(coordination.Task{
		ID:      "task-3",
		Info:    "write the report",
		Outcome: coordination.OutcomeDone,
	}, "verified", nil, 4*time.Minute+12*time.Second, 12)
	want := "[任务报告] task-3 · done · 耗时 4m12s\n目标: write the report\ntoken: 12\nverifier 输出:\nverified"
	if got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}

	got = formatReport(coordination.Task{
		ID:      "task-1",
		Info:    "goal",
		Outcome: coordination.OutcomeFailed,
	}, "", errors.New("planner boom"), time.Second, 0)
	if !strings.Contains(got, "planner boom") || !strings.Contains(got, "failed") {
		t.Fatalf("failed report = %q", got)
	}
}
