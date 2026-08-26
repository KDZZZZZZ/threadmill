// Package cmdcache 按推断出的依赖复用命令执行结果，包括产物。
//
// 缓存要回答两个不同的问题，都由系统调用追踪回答：
//   - 拿什么当键：读集，也就是这条命令实际依赖哪些文件；
//   - 存什么：写集，也就是它产出了哪些文件。
//
// 读集只能靠追踪。OverlayFS 的 upper 层只记录写，读是穿透到 lowerdir 的，
// 只被读过的文件不留任何痕迹；而 vfs.StateFingerprint 虽然安全，却是整棵树
// 的指纹，任何无关改动都会失效——多 agent 场景下几乎必然 miss，而跨 agent
// 复用正是这个包存在的理由。
//
// 写集本可以走 VFS 的 upper 层差分，这里同样取自追踪：追踪已经精确到路径，
// 读回产物的代价是 O(写集) 而不是 O(整棵树)，也不依赖 overlay 挂载成功。
//
// 一切降级都朝同一个方向：追踪不可用、不完整、或观测到无法回放的副作用时，
// 结果就不进缓存。只会少命中，不会错命中。
package cmdcache

import (
	"bufio"
	"errors"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// ReadKind 是一条读依赖的种类。
type ReadKind uint8

const (
	// ReadFile 是读过内容的文件，校验时比对内容哈希。
	ReadFile ReadKind = iota + 1
	// ReadAbsent 是探测过但不存在的路径，校验时要求仍然不存在。
	// 缺了它，别的 agent 新建该文件后会静默误命中。
	ReadAbsent
	// ReadDir 是以 O_DIRECTORY 打开的目录，校验时比对条目名集合而非内容。
	// `./...` 这类展开依赖的正是条目集合。
	ReadDir
	// ReadStat 是只被 stat/access 探过的路径，校验时只比对类型，不比对内容。
	//
	// 这个区分是命中率的关键。`go build .` 会枚举包目录并逐项 stat 判后缀，
	// README.md 因此出现在观测里——但它只被看了元数据，内容根本没读。
	// 把它记成内容依赖，别的 agent 编辑 README 就会让构建缓存失效，
	// 而那正是这个特性要消除的情形。
	ReadStat
)

// strength 决定同一路径的多次观测里哪条证据说了算。
// 后来的强证据可以升级先前的弱判定：目录几乎总是先被 stat 再被 opendir，
// 源文件也常常先被 stat 再被读取。
func (k ReadKind) strength() int {
	switch k {
	case ReadStat:
		return 1
	case ReadFile:
		return 2
	case ReadDir:
		return 3
	default:
		return 0
	}
}

// maxEscapedPaths 限制逃逸样本数量：它只用于诊断，不需要完整列表。
const maxEscapedPaths = 8

// maxTraceLine 是单行追踪的上限。strace 不截断路径参数，正常行远小于此值。
const maxTraceLine = 1 << 20

// Observation 是一次命令执行期间观测到的文件与网络行为。
// 路径都相对 live 树根，用斜杠分隔。
type Observation struct {
	// Reads 是推断出的依赖集，缓存键由它计算。
	Reads map[string]ReadKind
	// Writes 是观测到的写入路径，也就是要收进缓存的产物清单。
	// 它同时承担两件事：把命令自己产出的中间文件排除出读集，以及逃逸检测。
	Writes map[string]struct{}
	// Externals 是在工作区之外执行的二进制（宿主工具链），按 (size, mtime) 记依赖。
	Externals []string
	// Escaped 是写到 live 树与临时目录之外的路径样本。非空即不可缓存。
	Escaped []string
	// Network 表示观测到 AF_INET/AF_INET6 出站流量。为真即不可缓存。
	Network bool
	// Incomplete 表示追踪不可信（超限、截断、路径无法解析）。为真即不可缓存。
	Incomplete bool
}

// Cacheable 报告这次观测是否允许入缓存。
// 不靠命令名黑名单，只看观测到的实际行为。
func (o Observation) Cacheable() bool {
	return !o.Incomplete && !o.Network && len(o.Escaped) == 0 && !o.rewritesOwnInput()
}

// rewritesOwnInput 报告是否存在既读又写的路径。
//
// 读集必须记的是执行前的状态，可读集要等执行结束才知道——这是个先有鸡还是
// 先有蛋的问题。对纯输入路径不成问题：命令没改它们，执行后再哈希拿到的就是
// 执行前的值。但对既读又写的路径（`sed -i`、重写 lockfile）执行后的哈希是
// 被改过的内容，拿它当键必然匹配不上任何环境。这类命令直接不缓存。
//
// 首次触碰是写的路径不算冲突——解析时就没把它们放进读集，它们是产物不是依赖。
// 构建与测试类命令不改自己的输入，正是缓存要覆盖的主要目标。
func (o Observation) rewritesOwnInput() bool {
	for rel := range o.Writes {
		kind, isDependency := o.Reads[rel]
		if !isDependency {
			continue
		}
		// 「先探测到不存在，再创建」不是冲突：追踪已经精确给出了执行前的
		// 状态就是「不存在」，不需要在执行后回头去哈希它。
		// `go build -o app` 正是这个形状，它必须能缓存。
		if kind == ReadAbsent {
			continue
		}
		return true
	}
	return false
}

// hostMountPrefixes 是宿主只读绑定进沙箱的路径。bwrap 把 live 树绑在 `/`，
// 这些前缀盖在它上面，属于宿主而非工作区内容，不进读集也不算逃逸。
var hostMountPrefixes = []string{
	"/bin", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32",
	"/proc", "/run", "/sbin", "/sys", "/tmp", "/usr", "/var/run",
}

type pathRole uint8

const (
	roleRead pathRole = 1 << iota
	roleWrite
)

// syscallRoles 说明一个系统调用的路径参数各自扮演什么角色。
// first 用于第一个路径参数，rest 用于其余参数：rename 和 link 的源路径
// 既是依赖又被改动，目标路径只是写入。
type syscallRoles struct {
	first pathRole
	rest  pathRole
	// metadata 表示这个调用只观测得到元数据，读不到内容。
	// 由它建立的依赖只比对类型，内容变了不算变。
	metadata bool
}

var tracedSyscalls = map[string]syscallRoles{}

// contentReaders 真正读到了内容，依赖按内容哈希比对。
// readlink 归在这里：链接目标就是它的内容。
var contentReaders = []string{
	"open", "openat", "openat2",
	"readlink", "readlinkat",
	"execve", "execveat",
}

// metadataReaders 只看得到元数据。依赖只比对类型，别的 agent 改内容不算变。
// 这是 `go build .` 能在同事编辑 README 之后仍然命中的原因。
var metadataReaders = []string{
	"stat", "lstat", "stat64", "newfstatat", "fstatat64", "statx",
	"access", "faccessat", "faccessat2",
	"statfs", "getxattr", "lgetxattr", "listxattr", "llistxattr",
	"chdir",
}

// writers 改动路径本身。
var writers = []string{
	"creat", "unlink", "unlinkat", "rmdir", "mkdir", "mkdirat",
	"symlink", "symlinkat",
	"chmod", "fchmodat", "fchmodat2", "chown", "lchown", "fchownat",
	"truncate", "utime", "utimes", "utimensat", "futimesat",
	"mknod", "mknodat",
	"setxattr", "lsetxattr", "removexattr", "lremovexattr",
	"mount", "umount2",
}

// movers 的源路径既是依赖又被改动，目标路径只是写入。
var movers = []string{"rename", "renameat", "renameat2", "link", "linkat"}

func init() {
	for _, name := range contentReaders {
		tracedSyscalls[name] = syscallRoles{first: roleRead}
	}
	for _, name := range metadataReaders {
		tracedSyscalls[name] = syscallRoles{first: roleRead, metadata: true}
	}
	for _, name := range writers {
		tracedSyscalls[name] = syscallRoles{first: roleWrite}
	}
	for _, name := range movers {
		tracedSyscalls[name] = syscallRoles{first: roleRead | roleWrite, rest: roleWrite}
	}
}

// outboundSyscalls 是能产生出站流量的调用。socket() 本身不算，只有真正发出去才算。
var outboundSyscalls = map[string]bool{
	"connect": true, "sendto": true, "sendmsg": true, "sendmmsg": true,
}

// ParseTrace 解析 `strace -f -y` 的输出。
//
// root 是沙箱内 live 树的挂载点（bwrap 下就是 "/"），tmp 是沙箱内的 per-env
// 临时目录——它既不是依赖也不是产物，更不算逃逸。limit 是允许消费的字节数，
// 超出即把观测标记为不可信。
//
// 解析对错误一律 fail closed：无法解析的相对路径、被截断的路径、悬空的
// <unfinished> 都会置 Incomplete，让调用方放弃缓存本次结果。
func ParseTrace(r io.Reader, root, tmp string, limit int) (Observation, error) {
	p := &traceParser{
		obs: Observation{
			Reads:  make(map[string]ReadKind),
			Writes: make(map[string]struct{}),
		},
		root:      cleanSandboxPath(root),
		tmp:       cleanSandboxPath(tmp),
		cwd:       make(map[int]string),
		pending:   make(map[int]string),
		externals: make(map[string]struct{}),
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxTraceLine)
	consumed := 0
	for scanner.Scan() {
		line := scanner.Text()
		consumed += len(line) + 1
		if limit > 0 && consumed > limit {
			p.obs.Incomplete = true
			break
		}
		p.consume(line)
	}
	if err := scanner.Err(); err != nil {
		if !errors.Is(err, bufio.ErrTooLong) {
			return p.obs, err
		}
		// 超长行说明 strace 输出不是我们认识的形状，保守作废。
		p.obs.Incomplete = true
	}
	p.flushPending()
	p.obs.Externals = sortedKeys(p.externals)
	return p.obs, nil
}

// flushPending 处理进程退出时仍未收到结果的系统调用。
//
// 多进程构建里这几乎必然发生：一次 `go build` 能产生上万对
// unfinished/resumed，末尾总有一两个进程在系统调用途中就退出了。
// 为此作废整条追踪等于对所有真实构建关掉缓存。
//
// 参数是完整的，缺的只是结果，所以按成功处理——这是保守方向：
// 多记一条读依赖只会多几次 miss；写入路径的真实状态由记录阶段的 lstat
// 决定，不会凭空造出产物。
func (p *traceParser) flushPending() {
	for pid, head := range p.pending {
		delete(p.pending, pid)
		p.consume(strconv.Itoa(pid) + " " + head + ") = 0")
	}
}

type traceParser struct {
	obs       Observation
	root      string
	tmp       string
	cwd       map[int]string
	pending   map[int]string
	externals map[string]struct{}
}

func (p *traceParser) consume(line string) {
	pid, rest := splitPID(line)
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.HasPrefix(rest, "---") || strings.HasPrefix(rest, "+++") {
		return
	}
	if strings.HasPrefix(rest, "<...") {
		head, ok := p.pending[pid]
		if !ok {
			p.obs.Incomplete = true
			return
		}
		delete(p.pending, pid)
		marker := strings.Index(rest, "resumed>")
		if marker < 0 {
			p.obs.Incomplete = true
			return
		}
		rest = head + rest[marker+len("resumed>"):]
	}
	if strings.HasSuffix(rest, "<unfinished ...>") {
		p.pending[pid] = strings.TrimSuffix(rest, "<unfinished ...>")
		return
	}
	open := strings.IndexByte(rest, '(')
	if open <= 0 {
		return
	}
	name := strings.TrimSpace(rest[:open])
	end := strings.LastIndex(rest, ") = ")
	if end < open {
		// `= ?` 之类没有结果的行（exit_group、被信号打断的重启）不携带可用信息。
		return
	}
	args := rest[open+1 : end]
	result := strings.TrimSpace(rest[end+len(") = "):])

	if outboundSyscalls[name] && strings.Contains(args, "AF_INET") {
		p.obs.Network = true
	}
	roles, traced := tracedSyscalls[name]
	if !traced {
		return
	}
	tokens, truncated := scanArgs(args)
	if truncated {
		p.obs.Incomplete = true
		return
	}
	paths := p.resolvePaths(pid, tokens)
	if paths == nil {
		return
	}
	errno, failed := parseFailure(result)
	for i, abs := range paths {
		role := roles.first
		if i > 0 && roles.rest != 0 {
			role = roles.rest
		}
		if isOpen(name) {
			role = openRole(tokens.flags)
		}
		p.record(name, abs, role, tokens.flags, errno, failed, roles.metadata)
	}
	if name == "chdir" && !failed && len(paths) == 1 {
		p.cwd[pid] = paths[0]
	}
}

// record 把一次路径访问归入读集、写集、外部依赖或逃逸。
//
// 顺序规则很重要：同一路径的首次触碰决定它是不是依赖。命令自己产出的中间
// 文件（首次触碰是写）不能进读集，否则读集里记的是产物的执行后内容，任何
// 环境都匹配不上。反过来，先读后写的文件仍然是依赖。
func (p *traceParser) record(
	name, abs string,
	role pathRole,
	flags, errno string,
	failed, metadata bool,
) {
	// per-env 临时目录：既不是依赖也不是产物，更不算逃逸。
	// 放在最前面，免得命令在自己的 TMPDIR 里执行的脚本被记成宿主工具链依赖。
	if p.tmp != "" && underPath(abs, p.tmp) {
		return
	}
	onHostMount := underHostMount(p.root, abs)
	if !underPath(abs, p.root) || onHostMount {
		switch {
		case name == "execve" || name == "execveat":
			// 宿主工具链升级要能让缓存失效，按 (size, mtime) 记成外部依赖。
			p.externals[abs] = struct{}{}
		case onHostMount:
			// 宿主只读绑定与设备节点：/usr 是只读的，/dev 和 /proc 是内核接口。
			// 往这里写没有需要回放的副作用，`cmd > /dev/null` 不该因此失去缓存资格。
		case role&roleWrite != 0 && !failed:
			// 写到 live 树之外的真实副作用回放不了，命中方会默默丢失它。
			if len(p.obs.Escaped) < maxEscapedPaths {
				p.obs.Escaped = append(p.obs.Escaped, abs)
			}
		}
		return
	}
	rel := workspaceRel(p.root, abs)
	if rel == "" {
		return
	}
	// 失败的写没有改动任何东西，但确实探明了存在性，按读探测处理。
	if failed {
		role = roleRead
	}
	if role&roleRead != 0 {
		p.recordRead(rel, flags, errno, failed, metadata)
	}
	if role&roleWrite != 0 && !failed {
		p.obs.Writes[rel] = struct{}{}
	}
}

func (p *traceParser) recordRead(rel, flags, errno string, failed, metadata bool) {
	if _, written := p.obs.Writes[rel]; written {
		// 首次触碰是写：这是命令自己的产物，不是依赖。
		return
	}
	kind := ReadFile
	switch {
	case failed && (errno == "ENOENT" || errno == "ENOTDIR"):
		kind = ReadAbsent
	case strings.Contains(flags, "O_DIRECTORY"):
		kind = ReadDir
	case metadata:
		kind = ReadStat
	default:
		// 其余失败（EACCES、EISDIR……）保守当成「存在且内容相关」：
		// 多几次 miss，不会错命中。
	}
	previous, seen := p.obs.Reads[rel]
	if !seen {
		p.obs.Reads[rel] = kind
		return
	}
	// 已经确定不存在的路径不接受升级：那说明它是命令自己创建的，
	// 写集已经把它排除在依赖之外。
	if previous == ReadAbsent {
		return
	}
	if kind.strength() > previous.strength() {
		p.obs.Reads[rel] = kind
	}
}

// resolvePaths 把参数里的路径 token 解析成沙箱内的绝对路径。
// 相对路径优先按紧邻其前的 dirfd 标注解析，退回到该 pid 已知的 cwd；
// 都没有就说明观测缺了信息，整条追踪作废。
func (p *traceParser) resolvePaths(pid int, tokens argTokens) []string {
	if len(tokens.paths) == 0 {
		return nil
	}
	resolved := make([]string, 0, len(tokens.paths))
	for _, item := range tokens.paths {
		if strings.HasPrefix(item.value, "/") {
			abs := path.Clean(item.value)
			resolved = append(resolved, abs)
			if item.base != "" {
				p.cwd[pid] = item.base
			}
			continue
		}
		base := item.base
		if base == "" {
			base = p.cwd[pid]
		}
		if base == "" {
			p.obs.Incomplete = true
			return nil
		}
		resolved = append(resolved, path.Clean(path.Join(base, item.value)))
	}
	for _, item := range tokens.paths {
		if item.base != "" && item.fromCwd {
			p.cwd[pid] = item.base
		}
	}
	return resolved
}

type pathArg struct {
	value string
	// base 是该路径参数对应的 dirfd 标注（strace -y 打印的解析后目录）。
	base string
	// fromCwd 表示 base 来自 AT_FDCWD 标注，可以顺带更新该 pid 的 cwd。
	fromCwd bool
}

type argTokens struct {
	paths []pathArg
	flags string
}

// scanArgs 扫描一条系统调用的参数串，取出顶层的路径字符串、它们各自的
// dirfd 标注，以及顶层的裸 token（用于识别 O_* 标志）。
//
// 只取深度 0 的字符串，这样 execve 的 argv（在 [...] 里）和 sockaddr 结构
// （在 {...} 里）不会被当成路径。
func scanArgs(args string) (argTokens, bool) {
	var out argTokens
	var flags strings.Builder
	base := ""
	baseFromCwd := false
	depth := 0
	atFDCWD := false
	for i := 0; i < len(args); {
		switch c := args[i]; {
		case c == '"':
			value, next, truncated, ok := scanString(args, i)
			if !ok {
				return out, true
			}
			if depth == 0 {
				if truncated {
					return out, true
				}
				out.paths = append(out.paths, pathArg{value: value, base: base, fromCwd: baseFromCwd})
			}
			i = next
		case c == '<':
			closing := strings.IndexByte(args[i:], '>')
			if closing < 0 {
				return out, true
			}
			if depth == 0 {
				base = path.Clean(args[i+1 : i+closing])
				baseFromCwd = atFDCWD
			}
			i += closing + 1
		case c == '[' || c == '{' || c == '(':
			depth++
			i++
		case c == ']' || c == '}' || c == ')':
			depth--
			i++
		default:
			if depth == 0 {
				flags.WriteByte(c)
			}
			atFDCWD = strings.HasSuffix(flags.String(), "AT_FDCWD")
			i++
		}
	}
	out.flags = flags.String()
	return out, false
}

// scanString 读出从 i 处引号开始的字符串，解开 strace 的转义。
// truncated 表示 strace 在字符串后打了 `...` 省略标记。
func scanString(args string, i int) (value string, next int, truncated, ok bool) {
	var b strings.Builder
	i++
	for i < len(args) {
		switch args[i] {
		case '\\':
			if i+1 >= len(args) {
				return "", 0, false, false
			}
			decoded, width := unescape(args[i+1:])
			b.WriteString(decoded)
			i += 1 + width
		case '"':
			i++
			return b.String(), i, strings.HasPrefix(args[i:], "..."), true
		default:
			b.WriteByte(args[i])
			i++
		}
	}
	return "", 0, false, false
}

func unescape(s string) (string, int) {
	switch s[0] {
	case 'n':
		return "\n", 1
	case 't':
		return "\t", 1
	case 'r':
		return "\r", 1
	case '\\':
		return "\\", 1
	case '"':
		return "\"", 1
	}
	width := 0
	for width < 3 && width < len(s) && s[width] >= '0' && s[width] <= '7' {
		width++
	}
	if width == 0 {
		return s[:1], 1
	}
	code, err := strconv.ParseUint(s[:width], 8, 16)
	if err != nil {
		return s[:width], width
	}
	return string([]byte{byte(code)}), width
}

func isOpen(name string) bool {
	return name == "open" || name == "openat" || name == "openat2" || name == "creat"
}

// openRole 按 open 标志判定这次打开是读还是写。
func openRole(flags string) pathRole {
	writing := strings.Contains(flags, "O_WRONLY") ||
		strings.Contains(flags, "O_RDWR") ||
		strings.Contains(flags, "O_CREAT") ||
		strings.Contains(flags, "O_TRUNC") ||
		strings.Contains(flags, "O_APPEND")
	if !writing {
		return roleRead
	}
	// O_RDWR 但不新建不截断：内容既是输入也是输出。
	if strings.Contains(flags, "O_RDWR") &&
		!strings.Contains(flags, "O_CREAT") &&
		!strings.Contains(flags, "O_TRUNC") {
		return roleRead | roleWrite
	}
	return roleWrite
}

// parseFailure 从结果串里取出 errno 符号。errno 的文字描述随 locale 变化，
// 符号本身不变，所以只认符号。
func parseFailure(result string) (errno string, failed bool) {
	if !strings.HasPrefix(result, "-1") {
		return "", false
	}
	fields := strings.Fields(result)
	if len(fields) < 2 {
		return "", true
	}
	return fields[1], true
}

func splitPID(line string) (int, string) {
	trimmed := strings.TrimLeft(line, " \t")
	cut := 0
	for cut < len(trimmed) && trimmed[cut] >= '0' && trimmed[cut] <= '9' {
		cut++
	}
	if cut == 0 || cut >= len(trimmed) || trimmed[cut] != ' ' {
		return 0, trimmed
	}
	pid, err := strconv.Atoi(trimmed[:cut])
	if err != nil {
		return 0, trimmed
	}
	return pid, trimmed[cut+1:]
}

func cleanSandboxPath(p string) string {
	if p == "" {
		return ""
	}
	return path.Clean(p)
}

func underPath(abs, root string) bool {
	if root == "/" {
		return strings.HasPrefix(abs, "/")
	}
	return abs == root || strings.HasPrefix(abs, root+"/")
}

// underHostMount 只在 live 树被绑到 `/` 时生效：那时宿主的只读绑定盖在
// 工作区之上，必须按前缀排除，否则 /usr 下几百次动态库探测会灌进读集。
func underHostMount(root, abs string) bool {
	if root != "/" {
		return false
	}
	for _, prefix := range hostMountPrefixes {
		if underPath(abs, prefix) {
			return true
		}
	}
	return false
}

func workspaceRel(root, abs string) string {
	if root == "/" {
		return strings.TrimPrefix(abs, "/")
	}
	if abs == root {
		return ""
	}
	return strings.TrimPrefix(abs, root+"/")
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
