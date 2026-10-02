package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// traceProgram 是读集推断依赖的外部程序。
//
// OverlayFS 的 upper 层只记录写，读穿透到 lowerdir，不留下只读依赖的痕迹。
// 系统调用追踪提供命令实际访问的文件，供缓存校验依赖。
//
// 追踪不可用时缓存整体关闭，而不是退化成更宽的键：宁可不命中，
// 也不能在依赖不明的情况下复用结果。
const traceProgram = "strace"

// straceFlags 是追踪文件与网络访问的参数。
//
//	-D              让被执行命令保持调用方的直接子进程；后台后代不会拖住前台结果
//	-f              跟踪子进程：真正读文件的是编译器和测试进程，不是 shell
//	-y              打印 fd 对应的解析后路径，省掉自己跟踪 dirfd 与 chdir
//	--seccomp-bpf   让内核过滤非目标系统调用；追踪开销须单独实测
//	-e trace=...    %file 覆盖一切带路径的调用，%network 用来发现出站流量
//
// 不能加 `-e status=successful`：ENOENT 正是负依赖，探测过但不存在的路径
// 必须进读集，否则别的 agent 新建该文件后会静默误命中。
var straceFlags = []string{
	"-D",
	"-f",
	"-y",
	"--seccomp-bpf",
	"-e", "trace=%file,%network",
}

// traceRun 描述一次执行的追踪配置。
type traceRun struct {
	// program 是沙箱内可见的追踪器路径。
	program string
	// output 是沙箱内的追踪输出路径。
	output string
	// hostOutput 是同一个文件在宿主上的路径，执行结束后由调用方读取并删除。
	hostOutput string
	// root、tmp 和 home 是工作区、临时目录和用户目录的沙箱路径映射。
	root     string
	tmp      string
	home     string
	hostHome string
	// pgid 是被执行命令所在的进程组。-D 让 strace 留在该组但不再成为
	// 前台命令的父进程，因此前台退出后仍存活的组表示追踪尚未闭合。
	pgid int
	// incomplete 表示前台退出时仍有后代或 tracer 存活；它们后续的读写
	// 已超出本次命令事务，结果不能进入缓存。
	incomplete bool
}

const (
	traceClassifyDelay = 10 * time.Millisecond
	traceFlushTimeout  = time.Second
	traceFlushPoll     = time.Millisecond
)

func (t *traceRun) tracker(track func(int)) func(int) {
	if t == nil {
		return track
	}
	return func(pgid int) {
		t.pgid = pgid
		if track != nil {
			track(pgid)
		}
	}
}

// finish waits briefly for a normal tracer to flush and exit. A live descendant
// after that belongs to the environment lifecycle, not this cache transaction.
func (t *traceRun) finish() bool {
	if t == nil || t.pgid <= 0 {
		return true
	}
	if waitForProcessGroupExit(t.pgid, traceClassifyDelay) {
		return true
	}
	active, workload := processGroupState(t.pgid)
	if !active {
		return true
	}
	if workload {
		t.incomplete = true
		return false
	}
	if waitForProcessGroupExit(t.pgid, traceFlushTimeout) {
		return true
	}
	t.incomplete = true
	return false
}

func waitForProcessGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processGroupExists(pgid) {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(traceFlushPoll)
	}
	return true
}

func processGroupExists(pgid int) bool {
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// processGroupState ignores zombie/dead members. A live non-tracer after the
// foreground command exits is background workload, so a trace observation is
// incomplete without waiting for that workload to finish.
func processGroupState(pgid int) (active, workload bool) {
	if !processGroupExists(pgid) {
		return false, false
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true, true
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		text := string(data)
		end := strings.LastIndex(text, ") ")
		if end < 0 {
			continue
		}
		fields := strings.Fields(text[end+2:])
		if len(fields) < 3 {
			continue
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil || group != pgid {
			continue
		}
		if fields[0] == "Z" || fields[0] == "X" || fields[0] == "x" {
			continue
		}
		active = true
		name := text[strings.LastIndex(text[:end], "(")+1 : end]
		if name != traceProgram {
			return true, true
		}
	}
	return active, false
}

// wrap 把原本要执行的命令包进追踪器。
func (t *traceRun) wrap(argv []string) []string {
	if t == nil {
		return argv
	}
	wrapped := make([]string, 0, len(argv)+len(straceFlags)+4)
	wrapped = append(wrapped, t.program)
	wrapped = append(wrapped, straceFlags...)
	wrapped = append(wrapped, "-o", t.output, "--")
	return append(wrapped, argv...)
}

var (
	traceProbeOnce   sync.Once
	traceProbePath   string
	traceProbeReason string
)

// tracerPath 返回可用的追踪器路径，不可用返回空串。
//
// 先验证版本与实际 ptrace 启动能力；每个后端再验证沙箱内能力。
func tracerPath() string {
	traceProbeOnce.Do(func() {
		resolved, err := osexec.LookPath(traceProgram)
		if err != nil {
			traceProbeReason = "not_found"
			return
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			traceProbeReason = "not_found"
			return
		}
		if target, err := filepath.EvalSymlinks(abs); err == nil {
			abs = target
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		version, err := osexec.CommandContext(ctx, abs, "-V").Output()
		major, minor := 0, 0
		_, versionText, found := strings.Cut(string(version), "version ")
		parsed, _ := fmt.Sscanf(versionText, "%d.%d", &major, &minor)
		if err != nil || !found || parsed != 2 || major < 5 || major == 5 && minor < 3 {
			traceProbeReason = "unsupported_version"
			return
		}
		dir, err := os.MkdirTemp("", "threadmill-trace-probe-")
		if err != nil {
			traceProbeReason = "probe_failed"
			return
		}
		defer os.RemoveAll(dir)
		trace := &traceRun{program: abs, output: filepath.Join(dir, "trace")}
		defer func() {
			if trace.pgid <= 0 {
				return
			}
			if active, _ := processGroupState(trace.pgid); active {
				_ = syscall.Kill(-trace.pgid, syscall.SIGKILL)
				waitForProcessGroupExit(trace.pgid, traceClassifyDelay)
			}
		}()
		args := trace.wrap([]string{"/bin/sh", "-c", "true"})
		result, err := collect(ctx, osexec.CommandContext(ctx, args[0], args[1:]...), 1024, trace.tracker(nil))
		if err != nil || result.ExitCode != 0 || !trace.finish() {
			traceProbeReason = "probe_failed"
			return
		}
		info, statErr := os.Stat(trace.output)
		if statErr != nil || info.Size() == 0 {
			traceProbeReason = "probe_failed"
			return
		}
		traceProbePath = abs
	})
	return traceProbePath
}

// ProbeDependencyTracing tests tracing in the selected sandbox without a model
// call or enabling cache reuse. Cache-off benchmarks can use the same capability gate.
func (s *Scheduler) ProbeDependencyTracing(ctx context.Context) (ok bool, reason string) {
	if s == nil || s.sandbox == sandboxNone {
		return false, "sandbox_unavailable"
	}
	if s.sandbox == sandboxDocker {
		return false, "docker_tracing_unsupported"
	}
	program := tracerPath()
	if program == "" {
		return false, traceProbeReason
	}
	if s.sandbox == sandboxBwrap && !strings.HasPrefix(program, "/usr/") && !strings.HasPrefix(program, "/bin/") {
		return false, "tracer_outside_sandbox"
	}
	if s.externalWorkspaceIsolation && !s.externalWorkspaceIsolationAvailable {
		return false, "workspace_isolation_unavailable"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	live, err := os.MkdirTemp("", "threadmill-sandbox-trace-probe-")
	if err != nil {
		return false, "probe_failed"
	}
	defer os.RemoveAll(live)
	id := filepath.Base(live)
	defer func() {
		if err := s.Reap(id); err != nil {
			ok, reason = false, "probe_cleanup_failed"
		}
	}()
	result, trace, err := s.runSandboxed(ctx, vfs.NewStore(live), live, "true", id, true)
	defer trace.discard()
	if err != nil || result.ExitCode != 0 || trace == nil || trace.incomplete {
		return false, "probe_failed"
	}
	info, err := os.Stat(trace.hostOutput)
	if err != nil || info.Size() == 0 {
		return false, "probe_failed"
	}
	if obs, observed := trace.observe(); !observed || obs.Incomplete {
		return false, "probe_failed"
	}
	return true, ""
}

// newTraceRun 把追踪文件放在该环境的 tmp/，与可作为依赖的 home/ 分开。
func newTraceRun(program, live, runtimeDir, sandboxRoot, sandboxTmp, home string) (*traceRun, error) {
	if program == "" {
		return nil, nil
	}
	file, err := os.CreateTemp(filepath.Join(runtimeDir, "tmp"), ".tmtrace-")
	if err != nil {
		return nil, fmt.Errorf("exec: create trace file: %w", err)
	}
	name := filepath.Base(file.Name())
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("exec: create trace file: %w", err)
	}
	return &traceRun{
		program:    program,
		output:     sandboxTmp + "/" + name,
		hostOutput: file.Name(),
		root:       sandboxRoot,
		tmp:        sandboxTmp,
		home:       home,
		hostHome:   filepath.Join(runtimeDir, "home"),
	}, nil
}

func (t *traceRun) discard() {
	if t == nil {
		return
	}
	_ = os.Remove(t.hostOutput)
}

// cacheEnvHash 摘要影响执行结果、且跨环境稳定的变量。
//
// 有意排除 HOME 与 TMPDIR：它们是 per-env 的运行时目录，每个 agent 都不同，
// 算进键里会让缓存永远无法跨 agent 复用——而那正是这个特性存在的理由。
// HOME 的非内容寻址缓存区通过 ~/ 相对路径进入读集；TMPDIR 只供临时文件。
func cacheEnvHash(backend string, outputCap int) string {
	hasher := sha256.New()
	fmt.Fprintf(hasher, "backend\t%s\n", backend)
	fmt.Fprintf(hasher, "output\thead-tail-v1\t%d\n", outputCap)
	if backend == "bwrap" {
		fmt.Fprintln(hasher, "virtual-stat\ttype-v1")
		fmt.Fprintln(hasher, "fixed-eof\tdev-null-v1")
		fmt.Fprintf(hasher, "layout\t%s\t%s\t/tmp\n", bwrapWorkspace, sandboxHome)
		for _, path := range bwrapReadOnlyPaths {
			fmt.Fprintf(hasher, "ro-bind-try\t%s\n", path)
		}
	}
	fmt.Fprintf(hasher, "PATH\t%s\n", os.Getenv("PATH"))
	fmt.Fprintf(hasher, "LANG\tC.UTF-8\n")
	for _, name := range forwardedEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			fmt.Fprintf(hasher, "%s\t%s\n", name, value)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
