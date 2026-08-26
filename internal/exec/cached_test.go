package exec

import (
	"context"
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
	if cache.Stats().Stores != 1 {
		t.Fatalf("stores = %d, want 1; cache never recorded the run", cache.Stats().Stores)
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
	if cache.Stats().Hits != 1 {
		t.Fatalf("hits = %d, want 1", cache.Stats().Hits)
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
	if _, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 0 {
		t.Fatalf("hits = %d, want 0; a path that was absent at record time now exists", cache.Stats().Hits)
	}
}

// 不靠命令名黑名单：观测到出站流量就不缓存。
func TestCacheRejectsNetworkCommand(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	files := vfs.NewStore(baseRepo(t))

	// 连一个几乎肯定关着的本地端口：重点是发生了 AF_INET connect，不是它成功与否。
	const command = "bash -c 'exec 3<>/dev/tcp/127.0.0.1/9 && echo open' 2>/dev/null; echo done"

	if _, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{Command: command}); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Stores != 0 {
		t.Fatalf("stores = %d, want 0; observed outbound traffic must block caching", cache.Stats().Stores)
	}
	if cache.Stats().Rejected == 0 {
		t.Fatal("the run should have been rejected as uncacheable")
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
	if cache.Stats().Stores != 0 {
		t.Fatalf("stores = %d, want 0; a command that rewrites its input must not be cached", cache.Stats().Stores)
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
	if cache.Stats().Stores != 0 {
		t.Fatalf("stores = %d, want 0; a timeout must not be cached", cache.Stats().Stores)
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

// 真实构建的验收：一次实际的 go build，第二个 agent 必须拿到字节一致的
// 二进制，而且完全不占用执行槽位。这条测试是「复用执行结果包括产物」这句
// 话的证据，合成命令替代不了它。
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

	// GOTOOLCHAIN=local 阻止 go 去下载工具链：沙箱共享宿主网络，
	// 一次下载会让这条测试变成网络测试。GOPATH/GOCACHE 指向 per-env 临时目录。
	const command = "export GOTOOLCHAIN=local GOPATH=/tmp/gopath GOCACHE=/tmp/gocache && " +
		"go build -o app . && echo built"

	first, err := sched.View("agent-a", files).Run(context.Background(), env.Cmd{
		Command: command,
		Timeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ExitCode != 0 {
		t.Skipf("no usable Go toolchain inside the sandbox: %s", first.Output)
	}
	if cache.Stats().Stores != 1 {
		t.Fatalf("stores = %d, want 1; a real build must be cacheable", cache.Stats().Stores)
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

	second, err := sched.View("agent-b", files).Run(context.Background(), env.Cmd{
		Command: command,
		Timeout: 120 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cache.Stats().Hits != 1 {
		t.Fatalf("hits = %d, want 1; editing README must not invalidate a build", cache.Stats().Hits)
	}
	if second.ExitCode != 0 || second.Output != first.Output {
		t.Fatalf("cached result = (%d, %q), want (0, %q)", second.ExitCode, second.Output, first.Output)
	}
	if got := sched.Stats().Started; got != startedBefore {
		t.Fatalf("started = %d, want %d; a cache hit must not consume an execution slot", got, startedBefore)
	}
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
