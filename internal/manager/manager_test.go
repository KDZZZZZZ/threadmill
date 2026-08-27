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

// stateBlocksText 合并请求的稳定投影和已物化/待物化状态，供断言投影内容。
func stateBlocksText(request agent.Request) string {
	var b strings.Builder
	for _, block := range request.StableBlocks {
		b.WriteString(block.Text)
	}
	for _, message := range request.Messages {
		if message.ContextBlockID != "" {
			b.WriteString(message.Content)
		}
	}
	for _, block := range request.StateBlocks {
		b.WriteString(block.Text)
	}
	return b.String()
}

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

func TestOpenLoadsConfigOutsideProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "settings.yaml")
	data, err := os.ReadFile(filepath.Join("..", "..", provider.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := Open(context.Background(), Options{
		Root:       project,
		ConfigPath: configPath,
		Provider: stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "idle"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Close()
	if _, err := os.Stat(filepath.Join(project, provider.ConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("project config exists or stat failed: %v", err)
	}
}

func TestOpenUsesBuiltInConfigWithoutProjectFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()

	mgr, err := Open(context.Background(), Options{
		Root: project,
		Provider: stubProvider(func(context.Context, agent.Request) (agent.AssistantMessage, error) {
			return agent.AssistantMessage{Content: "idle"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Close()
	if _, err := os.Stat(filepath.Join(project, provider.ConfigFileName)); !os.IsNotExist(err) {
		t.Fatalf("project config exists or stat failed: %v", err)
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
		case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(request.SystemPrompt, "你是 manager"):
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
			case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			case strings.Contains(request.SystemPrompt, "你是 manager"):
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
			if strings.Contains(request.SystemPrompt, "你是 manager") {
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

func TestManagerBuffersRequestsWhileTurnIsRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	started := make(chan string, 3)
	releaseFirst := make(chan struct{})
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			}
			query := lastUser(request.Messages)
			started <- query
			if query == "first" {
				select {
				case <-releaseFirst:
				case <-callCtx.Done():
					return agent.AssistantMessage{}, callCtx.Err()
				}
			}
			return agent.AssistantMessage{Content: query + " done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	mgr.Send("first")
	if got := <-started; got != "first" {
		t.Fatalf("first manager request = %q, want first", got)
	}
	mgr.Send("second")
	mgr.Send("third")

	select {
	case got := <-started:
		t.Fatalf("manager started buffered request %q before current turn completed", got)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	for _, want := range []string{"second", "third"} {
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("manager request = %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestManagerHidesBufferedRequestFromCurrentTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	bufferObserved := make(chan bool, 1)
	dequeueObserved := make(chan bool, 1)
	mgr, err := Open(ctx, Options{
		Root: t.TempDir(),
		File: loadRepoConfig(t),
		Provider: stubProvider(func(callCtx context.Context, request agent.Request) (agent.AssistantMessage, error) {
			if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			}
			if lastUser(request.Messages) == "second" {
				dequeueObserved <- strings.Contains(stateBlocksText(request), "[User Message] second")
				return agent.AssistantMessage{Content: "done"}, nil
			}
			if hasToolResult(request.Messages) {
				bufferObserved <- strings.Contains(stateBlocksText(request), "[User Message] second")
				return agent.AssistantMessage{Content: "first done"}, nil
			}
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-callCtx.Done():
				return agent.AssistantMessage{}, callCtx.Err()
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "continue-first",
				Name:      "coordination_replacePending",
				Arguments: json.RawMessage(`{"roots":[],"spawns":[]}`),
			}}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	mgr.Send("first")
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	mgr.Send("second")
	close(releaseFirst)

	select {
	case leaked := <-bufferObserved:
		if leaked {
			t.Fatal("buffered request was visible to the current manager turn")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if visible := <-dequeueObserved; !visible {
		t.Fatal("buffered request was not projected when its manager turn started")
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
			case strings.Contains(request.SystemPrompt, "你是 manager"):
				managerOnce.Do(func() { close(managerStarted) })
				select {
				case <-releaseManager:
					return agent.AssistantMessage{Content: "manager recovered"}, nil
				case <-callCtx.Done():
					return agent.AssistantMessage{}, callCtx.Err()
				}
			case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
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
			case strings.Contains(request.SystemPrompt, "你是 manager"):
				return agent.AssistantMessage{Content: "reported"}, nil
			case strings.Contains(request.SystemPrompt, "你是记忆压缩器"):
				return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
			case strings.Contains(request.SystemPrompt, "你是 verifier"):
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
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		switch {
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 manager"):
			mu.Lock()
			query := lastUser(request.Messages)
			managerQueries = append(managerQueries, query)
			if strings.Contains(query, "[任务报告]") {
				managerReportMemory = stateBlocksText(request)
			} else if hasToolResult(request.Messages) {
				managerAfterGraphMemory = stateBlocksText(request)
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
		case strings.Contains(sys, "你是 planner"):
			if strings.Contains(lastUser(request.Messages), "CHILD INFO") {
				return agent.AssistantMessage{Content: "child plan"}, nil
			}
			return agent.AssistantMessage{Content: "root plan"}, nil
		case strings.Contains(sys, "你是 executor"):
			if response, ok := discardPendingJoin(request, "join:incoming:task-1:executor", "task-2"); ok {
				return response, nil
			}
			mu.Lock()
			if strings.Contains(lastUser(request.Messages), "[join pending]") {
				rootExecutorMemory = stateBlocksText(request)
			}
			mu.Unlock()
			if strings.Contains(lastUser(request.Messages), "child plan") {
				return agent.AssistantMessage{Content: "child did"}, nil
			}
			return agent.AssistantMessage{Content: "root did"}, nil
		default:
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
	if strings.Contains(rootExecutorMemory, "CHILD REPORT") {
		t.Fatalf("root executor prompt leaked candidate report: %q", rootExecutorMemory)
	}
	if !strings.Contains(rootExecutorMemory, "[User Message] USER MESSAGE") {
		t.Fatalf("root executor prompt = %q, want original user request", rootExecutorMemory)
	}
	joined := strings.Join(replies, "\n")
	if !strings.Contains(joined, "已建任务") || !strings.Contains(joined, "任务已完成") {
		t.Fatalf("replies = %q, want manager status then completion", replies)
	}
}

func TestManagerBuildsTaskPackageFromTaskInfoWithoutEagerRecall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created := false
	var organizerCalled bool
	var plannerMemory string
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 subgraph organizer"):
			organizerCalled = true
			return agent.AssistantMessage{Content: "unexpected"}, nil
		case strings.Contains(sys, "你是 manager"):
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
		case strings.Contains(sys, "你是 planner"):
			plannerMemory = stateBlocksText(request)
			return agent.AssistantMessage{Content: "plan"}, nil
		case strings.Contains(sys, "你是 executor"):
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
	if organizerCalled {
		t.Fatal("organizer ran before a role explicitly requested missing history")
	}
	if !strings.Contains(plannerMemory, "[Task Info] task-1: use NEED_TOKEN") {
		t.Fatalf("planner memory = %q, want protected task info", plannerMemory)
	}
	for _, unwanted := range []string{"NEEDED FACT NEED_TOKEN", "NOISE FACT"} {
		if strings.Contains(plannerMemory, unwanted) {
			t.Fatalf("planner memory = %q, contains unrequested history %q", plannerMemory, unwanted)
		}
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
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 manager"):
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(query, "[任务报告]"):
				managerFinalMemory = stateBlocksText(request)
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
				managerHelpMemory = stateBlocksText(request)
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
		case strings.Contains(sys, "你是 planner"):
			if strings.Contains(query, "gather evidence A") {
				return agent.AssistantMessage{Content: "helper-plan-A"}, nil
			}
			if strings.Contains(query, "gather evidence B") {
				return agent.AssistantMessage{Content: "helper-plan-B"}, nil
			}
			return agent.AssistantMessage{Content: "root-plan"}, nil
		case strings.Contains(sys, "你是 executor"):
			if strings.Contains(query, "helper-plan-A") {
				return agent.AssistantMessage{Content: "helper-did-A"}, nil
			}
			if strings.Contains(query, "helper-plan-B") {
				return agent.AssistantMessage{Content: "helper-did-B"}, nil
			}
			if response, ok := discardPendingJoin(
				request,
				"join:help:help/task-1:executor/need-help",
				"task-2",
				"task-3",
			); ok {
				return response, nil
			}
			if result := lastToolResult(request.Messages); result != "" {
				mu.Lock()
				requesterResult = result
				requesterMemory = stateBlocksText(request)
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
	if !strings.Contains(requesterResult, `"finished":true`) || !strings.Contains(requesterResult, "join:help:") {
		t.Fatalf("requester tool result = %q, want finished join session", requesterResult)
	}
	if strings.Contains(requesterMemory, "helper-result-A") || strings.Contains(requesterMemory, "helper-result-B") {
		t.Fatalf("requester prompt leaked helper reports outside join: %q", requesterMemory)
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

func TestManagerDeclinedHelpResumesRequester(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	rootCreated := false
	invalidHelpAttempted := false
	var requesterResult string
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 manager"):
			switch {
			case strings.Contains(query, "[拆分请求]"):
				if !invalidHelpAttempted {
					invalidHelpAttempted = true
					requestID := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(query, "\n", 2)[0], "[拆分请求]"))
					args, err := json.Marshal(map[string]any{
						"request_id": requestID,
						"spawns": []map[string]string{{
							"from": "task-1:executor", "info": "impossible self help",
						}},
					})
					if err != nil {
						return agent.AssistantMessage{}, err
					}
					return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
						ID:        "invalid-help",
						Name:      "coordination_provideHelp",
						Arguments: args,
					}}}, nil
				}
				return agent.AssistantMessage{Content: "当前没有可行的帮助分支"}, nil
			case strings.Contains(query, "[任务报告]"):
				return agent.AssistantMessage{Content: "任务已完成"}, nil
			case !rootCreated:
				rootCreated = true
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
		case strings.Contains(sys, "你是 planner"):
			return agent.AssistantMessage{Content: "root-plan"}, nil
		case strings.Contains(sys, "你是 executor"):
			if result := lastToolResult(request.Messages); result != "" {
				requesterResult = result
				return agent.AssistantMessage{Content: "executed"}, nil
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        "need-help",
				Name:      "coordination_requestHelp",
				Arguments: json.RawMessage(`{"reason":"need evidence"}`),
			}}}, nil
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
		t.Fatalf("Open() error = %v", err)
	}
	defer mgr.Close()

	mgr.Send("solve")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if !strings.Contains(requesterResult, "未提供帮助") {
		t.Fatalf("requester tool result = %q, want declined help result", requesterResult)
	}
	task := mgr.Snapshot().Tasks[0]
	if task.Outcome != coordination.OutcomeDone {
		t.Fatalf("task outcome = %q, want done", task.Outcome)
	}
}

func TestManagerUserCanAddFutureBranchWhileTaskRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plannerStarted := make(chan struct{})
	plannerRelease := make(chan struct{})
	graphChanged := make(chan struct{})
	childStarted := make(chan struct{})
	var plannerOnce sync.Once
	var changedOnce sync.Once
	var childOnce sync.Once
	var mu sync.Mutex
	rootCreated := false
	dynamicSubmitted := false
	provider := stubProvider(func(ctx context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 manager"):
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(query, "[任务报告]"):
				return agent.AssistantMessage{Content: "done"}, nil
			case query == "start" && !rootCreated:
				rootCreated = true
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:        "create-root",
					Name:      "coordination_replacePending",
					Arguments: json.RawMessage(`{"roots":[{"info":"root"}],"spawns":[]}`),
				}}}, nil
			case query == "add future" && !dynamicSubmitted:
				dynamicSubmitted = true
				return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
					ID:   "add-future",
					Name: "coordination_replacePending",
					Arguments: json.RawMessage(`{
						"roots":[{"info":"root"}],
						"spawns":[{
							"from":"task-1:executor",
							"join":"task-1:verifier",
							"info":"late child"
						}]
					}`),
				}}}, nil
			case query == "add future" && hasToolResult(request.Messages):
				changedOnce.Do(func() { close(graphChanged) })
				return agent.AssistantMessage{Content: "updated"}, nil
			default:
				return agent.AssistantMessage{Content: "started"}, nil
			}
		case strings.Contains(sys, "你是 planner"):
			if strings.Contains(query, "late child") {
				childOnce.Do(func() { close(childStarted) })
				return agent.AssistantMessage{Content: "child plan"}, nil
			}
			plannerOnce.Do(func() { close(plannerStarted) })
			select {
			case <-plannerRelease:
				return agent.AssistantMessage{Content: "root plan"}, nil
			case <-ctx.Done():
				return agent.AssistantMessage{}, ctx.Err()
			}
		case strings.Contains(sys, "你是 executor"):
			return agent.AssistantMessage{Content: "executed"}, nil
		default:
			if response, ok := discardPendingJoin(request, "join:incoming:task-1:verifier", "task-2"); ok {
				return response, nil
			}
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
	mgr.Send("start")
	select {
	case <-plannerStarted:
	case <-ctx.Done():
		t.Fatal("root planner did not start")
	}

	mgr.Send("add future")
	select {
	case <-graphChanged:
	case <-ctx.Done():
		t.Fatal("manager did not apply the running graph change")
	}
	if got := len(mgr.Snapshot().Tasks); got != 2 {
		t.Fatalf("tasks after running change = %d, want root and late child", got)
	}
	close(plannerRelease)
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-childStarted:
	default:
		t.Fatal("late child was added but never executed")
	}
	for _, task := range mgr.Snapshot().Tasks {
		if task.Outcome != coordination.OutcomeDone {
			t.Fatalf("task %s outcome = %q, want done", task.ID, task.Outcome)
		}
	}
}

func TestManagerIdleWhenOnlyTalking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
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
		if strings.Contains(request.SystemPrompt, "你是记忆压缩器") {
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
		if messages[i].Role == agent.RoleUser && messages[i].ContextBlockID == "" {
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

func discardPendingJoin(
	request agent.Request,
	sessionID string,
	sourceIDs ...string,
) (agent.AssistantMessage, bool) {
	query := lastUser(request.Messages)
	result := lastToolResult(request.Messages)
	switch {
	case strings.Contains(query, "[join pending]") && result == "",
		strings.Contains(result, "[join pending]"):
		args, _ := json.Marshal(map[string]any{
			"action":     "discard",
			"session_id": sessionID,
			"source_ids": sourceIDs,
			"reason":     "test candidate handled",
		})
		return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
			ID: "discard-join", Name: "join", Arguments: args,
		}}}, true
	case strings.Contains(result, `"discarded"`):
		args, _ := json.Marshal(map[string]any{
			"action": "finish", "session_id": sessionID, "reason": "test candidates handled",
		})
		return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
			ID: "finish-join", Name: "join", Arguments: args,
		}}}, true
	default:
		return agent.AssistantMessage{}, false
	}
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
	want = "[任务报告] task-1 · failed · 耗时 1s\n目标: goal\n流程错误:\nplanner boom"
	if got != want {
		t.Fatalf("failed report = %q, want %q", got, want)
	}
}

func TestManagerHoldsQueuedRootUntilReleased(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	plannerCalled := false
	provider := stubProvider(func(_ context.Context, request agent.Request) (agent.AssistantMessage, error) {
		sys := request.SystemPrompt
		query := lastUser(request.Messages)
		switch {
		case strings.Contains(sys, "你是记忆压缩器"):
			return agent.AssistantMessage{Content: `{"nodes":[]}`}, nil
		case strings.Contains(sys, "你是 manager"):
			if strings.Contains(query, "[任务报告]") || hasToolResult(request.Messages) {
				return agent.AssistantMessage{Content: "已处理"}, nil
			}
			policy := coordination.RunPolicyHeld
			callID := "hold"
			if strings.Contains(query, "RELEASE") {
				policy = coordination.RunPolicyEnabled
				callID = "release"
			}
			args, err := json.Marshal(coordination.PendingSubgraph{
				Roots: []coordination.PendingRoot{{Info: "HELD INFO", RunPolicy: policy}},
			})
			if err != nil {
				return agent.AssistantMessage{}, err
			}
			return agent.AssistantMessage{ToolCalls: []agenttool.Call{{
				ID:        callID,
				Name:      "coordination_replacePending",
				Arguments: args,
			}}}, nil
		case strings.Contains(sys, "你是 planner"):
			mu.Lock()
			plannerCalled = true
			mu.Unlock()
			return agent.AssistantMessage{Content: "plan"}, nil
		case strings.Contains(sys, "你是 executor"):
			return agent.AssistantMessage{Content: "did"}, nil
		default:
			return agent.AssistantMessage{Content: "REPORT"}, nil
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

	mgr.Send("QUEUE IT")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	snap := mgr.Snapshot()
	if len(snap.Tasks) != 1 || snap.Tasks[0].Outcome != coordination.OutcomeActive {
		t.Fatalf("tasks = %#v, want one active root", snap.Tasks)
	}
	if snap.Tasks[0].RunPolicy != coordination.RunPolicyHeld {
		t.Fatalf("run policy = %q, want %q", snap.Tasks[0].RunPolicy, coordination.RunPolicyHeld)
	}
	mu.Lock()
	started := plannerCalled
	mu.Unlock()
	if started {
		t.Fatal("held root started; want it to stay queued until released")
	}

	mgr.Send("RELEASE")
	if err := mgr.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle() after release error = %v", err)
	}
	snap = mgr.Snapshot()
	if snap.Tasks[0].Outcome != coordination.OutcomeDone {
		t.Fatalf("outcome = %q, want done after release", snap.Tasks[0].Outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if !plannerCalled {
		t.Fatal("released root never ran")
	}
}
