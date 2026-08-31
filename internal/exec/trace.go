package exec

import (
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
)

// traceProgram 是读集推断依赖的外部程序。
//
// 为什么必须是系统调用追踪：OverlayFS 的 upper 层只记录写，读是穿透到
// lowerdir 的，只被读过的文件不留任何痕迹。而 vfs.StateFingerprint 虽然安全，
// 却是整棵树的指纹，任何无关改动都会失效。要拿到「这条命令到底依赖哪些文件」，
// 在不改挂载层的前提下只有追踪这一条路。
//
// 追踪不可用时缓存整体关闭，而不是退化成更宽的键：宁可不命中，
// 也不能在依赖不明的情况下复用结果。
const traceProgram = "strace"

// straceFlags 是追踪文件与网络访问的参数。
//
//	-D              让被执行命令保持调用方的直接子进程；后台后代不会拖住前台结果
//	-f              跟踪子进程：真正读文件的是编译器和测试进程，不是 shell
//	-y              打印 fd 对应的解析后路径，省掉自己跟踪 dirfd 与 chdir
//	--seccomp-bpf   让内核过滤非目标系统调用，开销从数倍降到个位数百分比
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
	// root 与 tmp 是解析追踪时的沙箱路径映射：root 是 live 树的挂载点，
	// tmp 是 per-env 临时目录（既不是依赖也不是产物）。
	root string
	tmp  string
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
	traceProbeOnce sync.Once
	traceProbePath string
)

// tracerPath 返回可用的追踪器路径，不可用返回空串。
//
// 追踪器必须在沙箱内也能执行。bwrap 只把 /usr、/bin、/lib 这些目录只读绑进去，
// 装在别处的 strace 在沙箱里根本不存在，所以这里要求它位于被绑定的前缀下。
func tracerPath() string {
	traceProbeOnce.Do(func() {
		resolved, err := osexec.LookPath(traceProgram)
		if err != nil {
			return
		}
		abs, err := filepath.Abs(resolved)
		if err != nil {
			return
		}
		for _, prefix := range []string{"/usr/", "/bin/", "/sbin/"} {
			if strings.HasPrefix(abs, prefix) {
				traceProbePath = abs
				return
			}
		}
	})
	return traceProbePath
}

// newTraceRun 为一次执行准备追踪配置。tempDir 是该 env 的运行时目录，
// 在 bwrap 与 external 后端里都同时充当命令的 TMPDIR。
func newTraceRun(program, live, tempDir, sandboxRoot, sandboxTmp string) (*traceRun, error) {
	if program == "" {
		return nil, nil
	}
	file, err := os.CreateTemp(tempDir, ".tmtrace-")
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
// 它们的内容也不进读集，所以排除不会漏掉依赖。
func cacheEnvHash(backend string) string {
	hasher := sha256.New()
	fmt.Fprintf(hasher, "backend\t%s\n", backend)
	fmt.Fprintf(hasher, "PATH\t%s\n", os.Getenv("PATH"))
	fmt.Fprintf(hasher, "LANG\tC.UTF-8\n")
	for _, name := range networkEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			fmt.Fprintf(hasher, "%s\t%s\n", name, value)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
