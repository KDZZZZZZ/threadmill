package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// newCachedScheduler 建一个开了命令结果缓存的调度器。
// 追踪不可用的宿主直接跳过：本文件测的就是依赖推断本身。
func newCachedScheduler(t *testing.T, cfg Config) (*Scheduler, *cmdcache.Cache) {
	t.Helper()
	cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), CacheFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Cache = cache
	sched := New(cfg)
	if sched.sandbox == sandboxNone {
		t.Skip("no sandbox on this host")
	}
	if !sched.tracing {
		t.Skip("no usable syscall tracer on this host")
	}
	return sched, cache
}

func baseRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "input.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "README.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

// 这是整个特性的核心断言：两个 agent 从同一基线分叉，各自改了互不相干的
// 文件，同一条命令必须能复用彼此的执行结果和产物，而且命中的那次完全不占
// 执行槽位。
func TestCacheReusesResultAndArtifactAcrossAgents(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	const command = "cat input.txt > output.txt; echo built"

	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(first.Output) != "built" {
		t.Fatalf("first output = %q", first.Output)
	}
	if cache.Stats().Stores != 2 {
		t.Fatalf("stores = %d, want 2 command segments", cache.Stats().Stores)
	}

	// 第二个 agent 改了与命令无关的文件：整树指纹会 miss，读集不该 miss。
	if err := files.View("agent-b").Write("README.md", []byte("another agent edited this\n")); err != nil {
		t.Fatal(err)
	}
	startedBefore := sched.Stats().Started

	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 2 {
		t.Fatalf("hits = %d, want 2", cache.Stats().Hits)
	}
	if second.Output != first.Output {
		t.Fatalf("cached output = %q, want %q", second.Output, first.Output)
	}
	if got := sched.Stats().Started; got != startedBefore {
		t.Fatalf("started = %d, want %d; a cache hit must not consume an execution slot", got, startedBefore)
	}

	// 产物必须真的出现在命中方的工作区里。
	produced, err := files.View("agent-b").Read("output.txt")
	if err != nil {
		t.Fatalf("artifact missing in the hitting environment: %v", err)
	}
	if string(produced) != "payload\n" {
		t.Fatalf("artifact content = %q, want %q", produced, "payload\n")
	}
}

func TestCacheReusesSegmentAcrossDifferentWrappers(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: "cat input.txt > output.txt; printf first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 0 || first.Output != "first" {
		t.Fatalf("first result = %#v", first)
	}

	startedBefore := sched.Stats().Started
	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: "cat input.txt > output.txt\nprintf second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ExitCode != 0 || second.Output != "second" {
		t.Fatalf("second result = %#v", second)
	}
	if cache.Stats().Hits != 1 {
		t.Fatalf("cache stats = %+v, want one shared segment hit", cache.Stats())
	}
	if got := sched.Stats().Started - startedBefore; got != 1 {
		t.Fatalf("second request started %d times, want one admission despite segmented execution", got)
	}
	produced, err := files.View("agent-b").Read("output.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(produced) != "payload\n" {
		t.Fatalf("output.txt = %q, want payload", produced)
	}
}

func TestSegmentedCachePreservesShellListSemantics(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	const prefix = "false && printf skipped; printf 'status=%s;' $?; " +
		"true || printf skipped-too; false || printf recovered-"
	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: prefix + "; printf first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 0 || first.Output != "status=1;recovered-first" {
		t.Fatalf("first result = %#v", first)
	}
	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: prefix + "; printf second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ExitCode != 0 || second.Output != "status=1;recovered-second" {
		t.Fatalf("second result = %#v", second)
	}
	if cache.Stats().Hits == 0 {
		t.Fatal("shared shell-list segments did not hit the cache")
	}
}

func TestSegmentedCacheKeysExitStatusDependency(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))
	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: "true; printf 'status=%s' $?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Output != "status=0" {
		t.Fatalf("first output = %q", first.Output)
	}
	hitsBefore := cache.Stats().Hits
	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: "false; printf 'status=%s' $?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Output != "status=1" {
		t.Fatalf("second output = %q; cached status leaked across exit codes", second.Output)
	}
	if cache.Stats().Hits != hitsBefore {
		t.Fatal("a status-dependent segment hit an entry recorded for another exit code")
	}
}

func TestSegmentedCacheFallsBackForShellState(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantExit   int
		wantOutput string
		absentPath string
	}{
		{
			name:       "directory",
			command:    "mkdir -p sub; cd sub; pwd",
			wantOutput: "/sub",
		},
		{
			name:       "variable",
			command:    "value=kept; printf '%s' \"$value\"",
			wantOutput: "kept",
		},
		{
			name:       "exit",
			command:    "exit 7; touch should-not-exist",
			wantExit:   7,
			absentPath: "should-not-exist",
		},
		{
			name:       "dynamic command",
			command:    "mkdir -p nested; $(printf cd) nested; pwd",
			wantOutput: "/nested",
		},
		{
			name:       "compact printf assignment",
			command:    "printf -vIFS %s ,; printf '<%s>' $(printf a,b)",
			wantOutput: "<a><b>",
		},
		{
			name:       "arithmetic assignment",
			command:    `printf '%s;' "$((IFS=44))"; printf '<%s>' $(printf a4b)`,
			wantOutput: "44;<a><b>",
		},
		{
			name:       "completion state",
			command:    "complete -W 'one two' thing; complete -p thing >/dev/null && printf kept",
			wantOutput: "kept",
		},
		{
			name:       "loop variable",
			command:    "for item in one; do :; done; test -v item && printf kept",
			wantOutput: "kept",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, _ := newCachedScheduler(t, Config{Slots: 1})
			files := vfs.NewStore(baseRepo(t))
			result, err := sched.View("agent", files).Run(context.Background(), env.Cmd{Command: tt.command})
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != tt.wantExit {
				t.Fatalf("exit code = %d, want %d; output=%q", result.ExitCode, tt.wantExit, result.Output)
			}
			if tt.wantOutput != "" && !strings.HasSuffix(strings.TrimSpace(result.Output), tt.wantOutput) {
				t.Fatalf("output = %q, want suffix %q", result.Output, tt.wantOutput)
			}
			if tt.absentPath != "" {
				if _, err := files.View("agent").Stat(tt.absentPath); err == nil {
					t.Fatalf("%s should not exist", tt.absentPath)
				}
			}
		})
	}
}

func TestSegmentedCacheStopsAfterSignaledShell(t *testing.T) {
	cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), CacheFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	sched := New(Config{Slots: 1, Cache: cache, ExternalSandbox: true})
	// 假 runner 隔离宿主信号；负退出码就是 os/exec 对段 shell 自身被信号
	// 杀死的表达。
	sched.tracing = true
	calls := 0
	sched.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		calls++
		return env.ExecResult{ExitCode: -1, Output: "terminated"}, nil
	}
	files := vfs.NewStore(baseRepo(t))
	result, err := sched.View("agent", files).Run(context.Background(), env.Cmd{
		Command: "printf first; printf should-not-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.ExitCode != -1 || result.Output != "terminated" {
		t.Fatalf("calls=%d result=%#v, want the shell signal to terminate the list", calls, result)
	}
}

func TestSegmentedCacheKeepsOneOutputCap(t *testing.T) {
	sched, _ := newCachedScheduler(t, Config{Slots: 1, OutputCapKB: 1})
	files := vfs.NewStore(baseRepo(t))
	const command = "head -c 800 /dev/zero | tr '\\0' a; head -c 800 /dev/zero | tr '\\0' b"
	want := strings.Repeat("a", 800) + strings.Repeat("b", 224) + "\n[output truncated]"
	for index, envID := range []string{"agent-a", "agent-b"} {
		startedBefore := sched.Stats().Started
		result, err := sched.View(envID, files).Run(context.Background(), env.Cmd{Command: command})
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 0 || result.Output != want {
			t.Fatalf("%s result = (%d, %q), want one global cap", envID, result.ExitCode, result.Output)
		}
		if count := strings.Count(result.Output, "[output truncated]"); count != 1 {
			t.Fatalf("%s truncation markers = %d, want 1", envID, count)
		}
		if index == 1 && sched.Stats().Started != startedBefore {
			t.Fatal("an all-hit segmented request consumed an execution slot")
		}
	}
}

func TestSegmentedCacheSharesOneTimeout(t *testing.T) {
	sched, _ := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))
	started := time.Now()
	result, err := sched.View("agent", files).Run(context.Background(), env.Cmd{
		Command: "sleep 0.13; sleep 0.14",
		Timeout: 200 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() = %#v, %v; want one shared deadline", result, err)
	}
	if elapsed := time.Since(started); elapsed >= 350*time.Millisecond {
		t.Fatalf("segmented timeout took %v; timeout appears to have reset", elapsed)
	}
	stats := sched.Stats()
	if stats.Requests != 1 || stats.Started != 1 || stats.Completed != 1 || stats.TimedOut != 1 {
		t.Fatalf("scheduler stats = %+v, want one timed-out request", stats)
	}
}

// 改动命令真正依赖的文件必须 miss，否则缓存就是错的。
func TestCacheMissesWhenRealDependencyChanges(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	const command = "cat input.txt"

	if _, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if err := files.View("agent-b").Write("input.txt", []byte("different\n")); err != nil {
		t.Fatal(err)
	}
	result, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 0 {
		t.Fatalf("hits = %d, want 0; a changed dependency must miss", cache.Stats().Hits)
	}
	if strings.TrimSpace(result.Output) != "different" {
		t.Fatalf("output = %q, want the freshly computed value", result.Output)
	}
}

// 负依赖：记录时不存在的路径，在别的环境里出现了就必须 miss。
func TestCacheMissesWhenProbedPathAppears(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	const command = "cat optional.txt 2>/dev/null; echo done"

	if _, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if err := files.View("agent-b").Write("optional.txt", []byte("now here\n")); err != nil {
		t.Fatal(err)
	}
	result, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "now here\ndone\n" {
		t.Fatalf("output = %q, want fresh dependency output plus cached tail", result.Output)
	}
	if cache.Stats().Hits != 1 {
		t.Fatalf("hits = %d, want only the dependency-free tail to hit", cache.Stats().Hits)
	}
}

// 不靠命令名黑名单：观测到出站流量就不缓存。
func TestCacheRejectsNetworkCommand(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))

	// 连一个几乎肯定关着的本地端口：重点是发生了 AF_INET connect，不是它成功与否。
	const networkSegment = "bash -c 'exec 3<>/dev/tcp/127.0.0.1/9 && echo open' 2>/dev/null"
	const command = networkSegment + "; echo done"

	if _, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Stores != 1 {
		t.Fatalf("stores = %d, want only the offline tail", cache.Stats().Stores)
	}
	if cache.Stats().Rejected == 0 {
		t.Fatal("the network segment should have been rejected as uncacheable")
	}
	hitsBefore, startedBefore := cache.Stats().Hits, sched.Stats().Started
	if _, err := sched.View("agent-b", files).Run(
		context.Background(), env.Cmd{Command: networkSegment},
	); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != hitsBefore || sched.Stats().Started != startedBefore+1 {
		t.Fatal("the observed network segment was reused")
	}
}

// 重写自己输入的命令不能缓存：读集记的是执行后的内容，当键必然是错的。
func TestCacheRejectsCommandThatRewritesItsInput(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))

	const command = "cat input.txt > copy.tmp && cat copy.tmp >> input.txt"

	if _, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Stores != 1 {
		t.Fatalf("stores = %d, want only the pure copy segment", cache.Stats().Stores)
	}
	if cache.Stats().Rejected == 0 {
		t.Fatal("the append segment should have been rejected")
	}
	if err := files.View("agent-b").Write("copy.tmp", []byte("payload\n")); err != nil {
		t.Fatal(err)
	}
	hitsBefore, startedBefore := cache.Stats().Hits, sched.Stats().Started
	if _, err := sched.View("agent-b", files).Run(
		context.Background(), env.Cmd{Command: "cat copy.tmp >> input.txt"},
	); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != hitsBefore || sched.Stats().Started != startedBefore+1 {
		t.Fatal("the append segment was reused")
	}
}

// 非零退出码按配置缓存，但退出码和输出都要原样复用。
func TestCacheReusesFailure(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 2})
	files := vfs.NewStore(baseRepo(t))

	const command = "cat input.txt >/dev/null; echo failing; exit 3"

	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", first.ExitCode)
	}
	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 1 {
		t.Fatalf("hits = %d, want 1", cache.Stats().Hits)
	}
	if second.ExitCode != 3 || second.Output != first.Output {
		t.Fatalf("cached failure = (%d, %q), want (3, %q)", second.ExitCode, second.Output, first.Output)
	}
}

// 超时反映的是环境而不是命令的结果，绝不能固化成所有 agent 的既定结论。
func TestCacheRejectsTimedOutCommand(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))

	_, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: "cat input.txt >/dev/null; sleep 30",
		Timeout: 300 * 1e6, // 300ms
	})
	if err == nil {
		t.Fatal("expected the command to time out")
	}
	if cache.Stats().Stores != 1 {
		t.Fatalf("stores = %d, want only the completed prefix", cache.Stats().Stores)
	}
	hitsBefore, startedBefore := cache.Stats().Hits, sched.Stats().Started
	_, err = sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: "sleep 30",
		Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second timeout = %v, want DeadlineExceeded", err)
	}
	if cache.Stats().Hits != hitsBefore || sched.Stats().Started != startedBefore+1 {
		t.Fatal("the timed-out segment was reused")
	}
}

// 关掉缓存时执行链路必须完全不受影响。
func TestSchedulerWorksWithoutCache(t *testing.T) {
	sched := New(Config{Slots: 1})
	if sched.sandbox == sandboxNone {
		t.Skip("no sandbox on this host")
	}
	files := vfs.NewStore(baseRepo(t))
	result, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: "cat input.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Output) != "payload" {
		t.Fatalf("output = %q", result.Output)
	}
	if sched.Stats().DependencyTracing {
		t.Fatal("tracing must stay off when no cache is configured")
	}
}

// 真实构建的验收：包装命令不同时，第二个 agent 仍必须复用实际
// go build 段及其字节一致的二进制；只有新的廉价包装段真正执行。
func TestCacheReusesRealGoBuild(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 4})
	base := t.TempDir()
	// go 1.16 这个下限任何更新的工具链都接受，免得测试跟宿主版本绑死。
	mustWrite(t, base, "go.mod", "module demo\n\ngo 1.16\n")
	mustWrite(t, base, "main.go", `package main

import "fmt"

func main() { fmt.Println("built by threadmill") }
`)
	mustWrite(t, base, "README.md", "docs\n")
	files := vfs.NewStore(base)

	// env 只影响构建段，不引入跨段 shell 状态。GOTOOLCHAIN=local 阻止
	// 下载工具链，GOPATH/GOCACHE 指向 per-env 临时目录。
	const build = "env GOTOOLCHAIN=local GOPATH=/tmp/gopath GOCACHE=/tmp/gocache go build -o app ."

	coldStarted := time.Now()
	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: build + " && printf first",
		Timeout: 120 * time.Second,
	})
	coldDuration := time.Since(coldStarted)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 0 {
		t.Skipf("no usable Go toolchain inside the sandbox: %s", first.Output)
	}
	if first.Output != "first" {
		t.Fatalf("first output = %q, want first", first.Output)
	}
	if cache.Stats().Stores != 2 {
		t.Fatalf("stores = %d, want build and wrapper segments", cache.Stats().Stores)
	}
	goldenLive, err := files.Materialize("agent-a")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile(filepath.Join(goldenLive, "app"))
	if err != nil {
		t.Fatal(err)
	}

	// 第二个 agent 编辑了 README。go build 只 stat 过它，没读内容，
	// 所以这不该让构建缓存失效。
	if err := files.View("agent-b").Write("README.md", []byte("edited by another agent\n")); err != nil {
		t.Fatal(err)
	}
	startedBefore := sched.Stats().Started

	reuseStarted := time.Now()
	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: build + " && printf second",
		Timeout: 120 * time.Second,
	})
	reuseDuration := time.Since(reuseStarted)
	if err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 1 {
		t.Fatalf("hits = %d, want 1; editing README must not invalidate a build", cache.Stats().Hits)
	}
	if second.ExitCode != 0 || second.Output != "second" {
		t.Fatalf("second result = (%d, %q), want (0, second)", second.ExitCode, second.Output)
	}
	if got := sched.Stats().Started; got != startedBefore+1 {
		t.Fatalf("started = %d, want %d; only one wrapper-bearing request should be admitted", got, startedBefore+1)
	}
	cacheStats := cache.Stats()
	if materialized := cacheStats.ArtifactReflinks + cacheStats.ArtifactCopies; materialized == 0 {
		t.Fatal("the cached build did not replay its binary artifact")
	}
	t.Logf(
		"cold build=%v segmented reuse=%v saved command time=%v reflinks=%d/%dB copies=%d/%dB",
		coldDuration,
		reuseDuration,
		cacheStats.SavedDuration,
		cacheStats.ArtifactReflinks,
		cacheStats.ReflinkBytes,
		cacheStats.ArtifactCopies,
		cacheStats.CopiedBytes,
	)
	produced, err := files.View("agent-b").Read("app")
	if err != nil {
		t.Fatalf("the built binary is missing in the hitting environment: %v", err)
	}
	if string(produced) != string(golden) {
		t.Fatalf("replayed binary differs: %d bytes vs %d", len(produced), len(golden))
	}
}

func mustWrite(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
