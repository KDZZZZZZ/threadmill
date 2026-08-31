package cmdcache

import (
	"reflect"
	"strings"
	"testing"
)

const traceLimit = 1 << 20

// parseFixture 用 bwrap 的布局解析一段 strace 输出：live 树挂在 /，临时目录在 /tmp。
func parseFixture(t *testing.T, lines string) Observation {
	t.Helper()
	obs, err := ParseTrace(strings.NewReader(lines), "/", "/tmp", traceLimit)
	if err != nil {
		t.Fatalf("ParseTrace: %v", err)
	}
	return obs
}

func TestParseTraceRecordsSuccessfulRead(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "go.mod", O_RDONLY|O_CLOEXEC) = 3</go.mod>
`)
	if got := obs.Reads["go.mod"]; got != ReadFile {
		t.Fatalf("go.mod read kind = %v, want ReadFile", got)
	}
	if obs.Incomplete {
		t.Fatal("trace should be complete")
	}
}

// 负依赖：探测过但不存在的路径必须进读集，否则别的 agent 新建该文件后会静默误命中。
func TestParseTraceRecordsAbsentPathAsDependency(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</internal/vfs>, "testdata", O_RDONLY) = -1 ENOENT (No such file or directory)
`)
	if got := obs.Reads["internal/vfs/testdata"]; got != ReadAbsent {
		t.Fatalf("testdata read kind = %v, want ReadAbsent", got)
	}
}

// 目录依赖：./... 这类展开依赖的是条目集合，不是任何单个文件的内容。
func TestParseTraceRecordsDirectoryOpen(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "internal", O_RDONLY|O_NONBLOCK|O_CLOEXEC|O_DIRECTORY) = 3</internal>
`)
	if got := obs.Reads["internal"]; got != ReadDir {
		t.Fatalf("internal read kind = %v, want ReadDir", got)
	}
}

// 命令自己产出的中间文件不是依赖：首次触碰是写就不能进读集，
// 否则读集里会记下产物的执行后内容，任何环境都无法匹配。
func TestParseTraceExcludesSelfWrittenFileFromReads(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "out.bin", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 3</out.bin>
1 openat(AT_FDCWD</>, "out.bin", O_RDONLY) = 4</out.bin>
`)
	if kind, ok := obs.Reads["out.bin"]; ok {
		t.Fatalf("out.bin must not be a dependency, got %v", kind)
	}
	if _, ok := obs.Writes["out.bin"]; !ok {
		t.Fatal("out.bin should be in the write set")
	}
}

// 先读后写的文件仍然是依赖：写入不能抹掉已经建立的读依赖。
func TestParseTraceKeepsDependencyWhenReadBeforeWrite(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "notes.md", O_RDONLY) = 3</notes.md>
1 openat(AT_FDCWD</>, "notes.md", O_WRONLY|O_TRUNC) = 4</notes.md>
`)
	if got := obs.Reads["notes.md"]; got != ReadFile {
		t.Fatalf("notes.md read kind = %v, want ReadFile", got)
	}
	if _, ok := obs.Writes["notes.md"]; !ok {
		t.Fatal("notes.md should also be in the write set")
	}
}

func TestParseTraceResolvesRelativePathAgainstAnnotatedCwd(t *testing.T) {
	obs := parseFixture(t, `1 chdir("/sub") = 0
1 access("b.txt", R_OK) = 0
`)
	// access 只看得到元数据，所以是 ReadStat 而不是 ReadFile。
	if got := obs.Reads["sub/b.txt"]; got != ReadStat {
		t.Fatalf("sub/b.txt read kind = %v, want ReadStat", got)
	}
}

// 宿主只读绑定进来的 /usr、/etc 不属于工作区，不能进读集。
func TestParseTraceIgnoresPathsOutsideWorkspace(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "/usr/lib/x86_64-linux-gnu/libc.so.6", O_RDONLY|O_CLOEXEC) = 3</usr/lib/x86_64-linux-gnu/libc.so.6>
1 openat(AT_FDCWD</>, "/etc/ld.so.cache", O_RDONLY|O_CLOEXEC) = 3</etc/ld.so.cache>
`)
	if len(obs.Reads) != 0 {
		t.Fatalf("reads = %v, want empty", obs.Reads)
	}
	if obs.Incomplete {
		t.Fatal("host paths must not mark the trace incomplete")
	}
}

// $TMPDIR 是 per-env 的运行时目录，既不是依赖也不是产物，更不算逃逸。
func TestParseTraceIgnoresTempDir(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "/tmp/go-build123/x.o", O_WRONLY|O_CREAT, 0666) = 3</tmp/go-build123/x.o>
`)
	if len(obs.Reads) != 0 || len(obs.Writes) != 0 || len(obs.Escaped) != 0 {
		t.Fatalf("tmp traffic leaked: reads=%v writes=%v escaped=%v", obs.Reads, obs.Writes, obs.Escaped)
	}
}

// 逃逸检测：写到 live 树和 tmp 之外的路径无法回放，该次结果不能进缓存。
func TestParseTraceDetectsEscapingWrite(t *testing.T) {
	obs, err := ParseTrace(strings.NewReader(
		`1 openat(AT_FDCWD</live>, "/home/oops/.gitconfig", O_WRONLY|O_CREAT, 0600) = 3</home/oops/.gitconfig>
`), "/live", "/live/.tmp", traceLimit)
	if err != nil {
		t.Fatalf("ParseTrace: %v", err)
	}
	if len(obs.Escaped) == 0 {
		t.Fatal("write outside the live tree should be reported as escaped")
	}
}

// 不靠命令名黑名单，靠观测到的实际网络行为判定不可缓存。
func TestParseTraceDetectsInetConnect(t *testing.T) {
	obs := parseFixture(t, `1 connect(3<TCP:[1234]>, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("1.2.3.4")}, 16) = 0
`)
	if !obs.Network {
		t.Fatal("AF_INET connect should set Network")
	}
}

// 本地 unix socket 不是不确定性来源，不应该把整条命令判成不可缓存。
func TestParseTraceIgnoresUnixSocket(t *testing.T) {
	obs := parseFixture(t, `1 connect(3<UNIX:[5678]>, {sa_family=AF_UNIX, sun_path="/run/nscd/socket"}, 110) = -1 ENOENT (No such file or directory)
`)
	if obs.Network {
		t.Fatal("AF_UNIX connect must not set Network")
	}
}

// execve 的目标不在工作区内时按 (size, mtime) 记成外部依赖，捕捉宿主工具链升级。
func TestParseTraceRecordsExternalExecutable(t *testing.T) {
	obs := parseFixture(t, `1 execve("/usr/bin/go", ["go", "test", "./..."], 0x7ffd /* 62 vars */) = 0
`)
	if !reflect.DeepEqual(obs.Externals, []string{"/usr/bin/go"}) {
		t.Fatalf("externals = %v, want [/usr/bin/go]", obs.Externals)
	}
}

// execve 的 argv 在 [...] 里，不能被当成路径参数解析出来。
func TestParseTraceDoesNotTreatArgvAsPaths(t *testing.T) {
	obs := parseFixture(t, `1 execve("/usr/bin/cat", ["cat", "a.txt"], 0x7ffd /* 62 vars */) = 0
`)
	if _, ok := obs.Reads["a.txt"]; ok {
		t.Fatalf("argv entry leaked into reads: %v", obs.Reads)
	}
}

func TestParseTraceJoinsUnfinishedAndResumed(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "slow.txt", O_RDONLY <unfinished ...>
2 openat(AT_FDCWD</>, "other.txt", O_RDONLY) = 4</other.txt>
1 <... openat resumed>) = 3</slow.txt>
`)
	if got := obs.Reads["slow.txt"]; got != ReadFile {
		t.Fatalf("slow.txt read kind = %v, want ReadFile", got)
	}
	if got := obs.Reads["other.txt"]; got != ReadFile {
		t.Fatalf("other.txt read kind = %v, want ReadFile", got)
	}
}

func TestParseTraceIgnoresSignalAndExitLines(t *testing.T) {
	obs := parseFixture(t, `1 --- SIGCHLD {si_signo=SIGCHLD, si_code=CLD_EXITED} ---
1 +++ exited with 0 +++
`)
	if len(obs.Reads) != 0 || obs.Incomplete {
		t.Fatalf("reads=%v incomplete=%v, want empty and complete", obs.Reads, obs.Incomplete)
	}
}

// 解析不出来的相对路径必须让整条追踪失效：宁可不缓存，也不能漏依赖。
func TestParseTraceMarksIncompleteOnUnresolvableRelativePath(t *testing.T) {
	obs := parseFixture(t, `1 access("relative.txt", R_OK) = 0
`)
	if !obs.Incomplete {
		t.Fatal("unresolvable relative path should mark the trace incomplete")
	}
}

func TestParseTraceIgnoresEmptyFstatatPathWithoutFlag(t *testing.T) {
	obs := parseFixture(t, `1 newfstatat(AT_FDCWD, "", 0x7ffd, 0) = -1 ENOENT (No such file or directory)
`)
	if obs.Incomplete {
		t.Fatal("an invalid empty path must not make the trace incomplete")
	}
	if len(obs.Reads) != 0 {
		t.Fatalf("reads = %v, want empty", obs.Reads)
	}
}

func TestParseTraceResolvesEmptyFstatatPathWithFlag(t *testing.T) {
	obs := parseFixture(t, `1 newfstatat(3</input.txt>, "", 0x7ffd, AT_EMPTY_PATH) = 0
`)
	if obs.Incomplete {
		t.Fatal("AT_EMPTY_PATH trace should be complete")
	}
	if got := obs.Reads["input.txt"]; got != ReadStat {
		t.Fatalf("input.txt read kind = %v, want ReadStat", got)
	}
}

// 超过上限的追踪同样失效，避免为一条巨型构建保留无界状态。
func TestParseTraceMarksIncompleteWhenOverLimit(t *testing.T) {
	line := `1 openat(AT_FDCWD</>, "go.mod", O_RDONLY) = 3</go.mod>` + "\n"
	obs, err := ParseTrace(strings.NewReader(strings.Repeat(line, 100)), "/", "/tmp", 64)
	if err != nil {
		t.Fatalf("ParseTrace: %v", err)
	}
	if !obs.Incomplete {
		t.Fatal("over-limit trace should be marked incomplete")
	}
}

func TestParseTraceRecordsUnlinkAsWrite(t *testing.T) {
	obs := parseFixture(t, `1 unlinkat(AT_FDCWD</>, "stale.txt", 0) = 0
`)
	if _, ok := obs.Writes["stale.txt"]; !ok {
		t.Fatalf("writes = %v, want stale.txt", obs.Writes)
	}
}

// rename 的两个路径都要算写入，源路径同时是依赖。
func TestParseTraceRecordsRenameBothSides(t *testing.T) {
	obs := parseFixture(t, `1 renameat2(AT_FDCWD</>, "old.txt", AT_FDCWD</sub>, "new.txt", RENAME_NOREPLACE) = 0
`)
	if _, ok := obs.Writes["old.txt"]; !ok {
		t.Fatalf("writes = %v, want old.txt", obs.Writes)
	}
	if _, ok := obs.Writes["sub/new.txt"]; !ok {
		t.Fatalf("writes = %v, want sub/new.txt", obs.Writes)
	}
	if got := obs.Reads["old.txt"]; got != ReadFile {
		t.Fatalf("old.txt read kind = %v, want ReadFile", got)
	}
}

// 目录几乎总是先被 stat 再被 opendir，首次触碰规则会把它定成 ReadFile。
// O_DIRECTORY 是权威的类型证据，必须能升级先前的判定，否则 ./... 展开
// 涉及的每个目录都会永久 miss。
func TestParseTraceUpgradesStattedDirectoryToReadDir(t *testing.T) {
	obs := parseFixture(t, `1 statx(AT_FDCWD</>, "internal", AT_STATX_SYNC_AS_STAT, STATX_MODE, {stx_mode=S_IFDIR|0775, ...}) = 0
1 openat(AT_FDCWD</>, "internal", O_RDONLY|O_CLOEXEC|O_DIRECTORY) = 3</internal>
`)
	if got := obs.Reads["internal"]; got != ReadDir {
		t.Fatalf("internal read kind = %v, want ReadDir", got)
	}
}

// 升级只朝一个方向走：已知不存在的路径不能被后来的目录打开改写，
// 那种情况说明路径是命令自己创建的，写集会负责排除它。
func TestParseTraceDoesNotUpgradeAbsentPath(t *testing.T) {
	obs := parseFixture(t, `1 newfstatat(AT_FDCWD</>, "build", 0x7ffd, 0) = -1 ENOENT (No such file or directory)
1 mkdir(AT_FDCWD</>, "build", 0755) = 0
1 openat(AT_FDCWD</>, "build", O_RDONLY|O_DIRECTORY) = 3</build>
`)
	if got := obs.Reads["build"]; got != ReadAbsent {
		t.Fatalf("build read kind = %v, want ReadAbsent", got)
	}
}

// 读集必须是执行前的状态，而读集要等执行完才知道。对既读又写的路径，
// 执行后再哈希拿到的是被改过的内容，当键必然错。这类命令一律不缓存。
func TestObservationRejectsReadWriteConflict(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "package-lock.json", O_RDONLY) = 3</package-lock.json>
1 openat(AT_FDCWD</>, "package-lock.json", O_WRONLY|O_TRUNC) = 4</package-lock.json>
`)
	if obs.Cacheable() {
		t.Fatal("a command that rewrites its own input must not be cacheable")
	}
}

func TestObservationRejectsAppendWithoutExplicitRead(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "report.txt", O_WRONLY|O_CREAT|O_APPEND, 0666) = 3</report.txt>
`)
	if obs.Cacheable() {
		t.Fatal("append depends on the destination's prior content and must not be cacheable")
	}
	if got := obs.Reads["report.txt"]; got != ReadFile {
		t.Fatalf("report.txt read kind = %v, want ReadFile", got)
	}
}

func TestObservationAllowsExclusiveCreate(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "object.tmp", O_WRONLY|O_CREAT|O_EXCL, 0666) = 3</object.tmp>
`)
	if !obs.Cacheable() {
		t.Fatal("successful exclusive create has an exact absent precondition and should be cacheable")
	}
	if got := obs.Reads["object.tmp"]; got != ReadAbsent {
		t.Fatalf("object.tmp read kind = %v, want ReadAbsent", got)
	}
}

func TestObservationRejectsNetworkAndEscape(t *testing.T) {
	network := Observation{Reads: map[string]ReadKind{}, Writes: map[string]struct{}{}, Network: true}
	if network.Cacheable() {
		t.Fatal("network traffic must block caching")
	}
	escaped := Observation{Reads: map[string]ReadKind{}, Writes: map[string]struct{}{}, Escaped: []string{"/home/x"}}
	if escaped.Cacheable() {
		t.Fatal("escaping writes must block caching")
	}
	incomplete := Observation{Reads: map[string]ReadKind{}, Writes: map[string]struct{}{}, Incomplete: true}
	if incomplete.Cacheable() {
		t.Fatal("an incomplete trace must block caching")
	}
}

// 纯构建/测试类命令不改自己的输入，必须仍然可缓存。
func TestObservationAllowsPureBuild(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "main.go", O_RDONLY) = 3</main.go>
1 openat(AT_FDCWD</>, "app", O_WRONLY|O_CREAT|O_TRUNC, 0777) = 4</app>
`)
	if !obs.Cacheable() {
		t.Fatalf("pure build should be cacheable: %+v", obs)
	}
}

// 往 /dev/null、/proc 这类宿主挂载点写没有需要回放的副作用：
// /usr 是只读绑定，/dev 和 /proc 是内核接口。`cmd > /dev/null` 极其常见，
// 把它判成逃逸会让一大批命令白白失去缓存资格。
func TestParseTraceDoesNotTreatHostMountWriteAsEscape(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "/dev/null", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 3</dev/null>
`)
	if len(obs.Escaped) != 0 {
		t.Fatalf("escaped = %v, want empty", obs.Escaped)
	}
	if !obs.Cacheable() {
		t.Fatal("writing to /dev/null must not block caching")
	}
	if len(obs.Writes) != 0 {
		t.Fatalf("writes = %v, want empty", obs.Writes)
	}
}

func TestParseTraceExcludesHostMountsBelowDedicatedWorkspace(t *testing.T) {
	obs, err := ParseTrace(strings.NewReader(
		`1 openat(AT_FDCWD</workspace>, "/dev/null", O_WRONLY|O_CREAT|O_TRUNC, 0666) = 3</dev/null>
`), "/workspace", "/tmp", traceLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Escaped) != 0 || !obs.Cacheable() {
		t.Fatalf("dedicated workspace classified /dev/null as escape: %+v", obs)
	}
}

// 但真实的树外写入仍然必须拦下。
func TestParseTraceStillDetectsRealEscape(t *testing.T) {
	obs, err := ParseTrace(strings.NewReader(
		`1 openat(AT_FDCWD</live>, "/home/oops/.cache/state", O_WRONLY|O_CREAT, 0644) = 3</home/oops/.cache/state>
`), "/live", "/live/.tmp", traceLimit)
	if err != nil {
		t.Fatalf("ParseTrace: %v", err)
	}
	if len(obs.Escaped) == 0 {
		t.Fatal("a real write outside the workspace must still be caught")
	}
}

// 多进程构建里几乎必然有进程在系统调用途中退出，留下悬空的 <unfinished ...>。
// 一次 go build 能产生上万对 unfinished/resumed，为末尾一两个悬空作废整条
// 追踪等于对所有真实构建关掉缓存。参数是完整的，按成功处理即可。
func TestParseTraceTreatsDanglingUnfinishedAsComplete(t *testing.T) {
	obs := parseFixture(t, `1 openat(AT_FDCWD</>, "main.go", O_RDONLY) = 3</main.go>
2 openat(AT_FDCWD</>, "killed.go", O_RDONLY <unfinished ...>
`)
	if obs.Incomplete {
		t.Fatal("a dangling unfinished call must not void the whole trace")
	}
	if got := obs.Reads["killed.go"]; got != ReadFile {
		t.Fatalf("killed.go read kind = %v, want ReadFile", got)
	}
}

// `go build -o app` 先探测 app 不存在再创建它。这不是读写冲突：
// 追踪已经精确给出执行前状态是「不存在」，不需要在执行后回头哈希。
func TestObservationAllowsProbeThenCreate(t *testing.T) {
	obs := parseFixture(t, `1 newfstatat(AT_FDCWD</>, "app", 0x7ffd, 0) = -1 ENOENT (No such file or directory)
1 openat(AT_FDCWD</>, "app", O_WRONLY|O_CREAT|O_TRUNC, 0777) = 3</app>
`)
	if !obs.Cacheable() {
		t.Fatal("probe-then-create is the shape of every build; it must be cacheable")
	}
	if got := obs.Reads["app"]; got != ReadAbsent {
		t.Fatalf("app read kind = %v, want ReadAbsent", got)
	}
	if _, ok := obs.Writes["app"]; !ok {
		t.Fatal("app should still be an artifact")
	}
}
