package cmdcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Key 是一条命令在缓存里的归类依据。读集不在 Key 里：读集要靠逐条校验
// 才能判定，而 Key 只负责把候选条目缩到同一命令、同一后端、同一环境变量。
type Key struct {
	// Command 是原始命令串。
	Command string
	// Backend 是执行后端（bwrap / docker / external）。
	// 不同后端的可见文件系统和网络策略不同，结果不可互换。
	Backend string
	// EnvHash 是影响执行的环境变量摘要。
	EnvHash string
	// Role attributes observations; it never partitions reusable results.
	Role string
	// ExternalPath maps a sandbox path to its visible host backing path.
	// Nil is identity; empty means the path is absent from that sandbox view.
	ExternalPath func(string) string
	// ExternalType describes structural virtual nodes with no host backing.
	// Only metadata observations may use this type; content reads fail closed.
	ExternalType func(string) string
}

func (k Key) index() string {
	// Older entries omit HOME and external reads and cannot be trusted.
	sum := sha256.Sum256([]byte("tmcmd3\n" + k.Command + "\n" + k.Backend + "\n" + k.EnvHash))
	return hex.EncodeToString(sum[:])
}

// ChangeKind 是一条产物变更的类型。
type ChangeKind string

const (
	ChangeFile    ChangeKind = "file"
	ChangeDir     ChangeKind = "dir"
	ChangeSymlink ChangeKind = "symlink"
	ChangeDelete  ChangeKind = "delete"
)

// Change 是一条要回放到 live 树的产物变更。
type Change struct {
	Path       string     `json:"path"`
	Kind       ChangeKind `json:"kind"`
	Digest     string     `json:"digest,omitempty"`
	Target     string     `json:"target,omitempty"`
	Executable bool       `json:"executable,omitempty"`
}

// Entry 是一次可复用的执行结果。
//
// 命中条件只有一条：Reads 和 Externals 里每个路径当前的状态串，都等于
// 记录时的状态串。写集不需要额外前置条件——首次触碰是写的路径，命令本来
// 就会无条件覆盖，回放覆盖它与真跑一遍等价。
type Entry struct {
	// ID 由 Reads 与 Externals 唯一决定，同样的依赖状态只会存一份。
	ID string `json:"-"`

	Command     string `json:"command"`
	Backend     string `json:"backend"`
	EnvHash     string `json:"env_hash"`
	CreatorRole string `json:"creator_role"`

	// Reads 是推断出的依赖：工作区相对路径 → 执行前状态串。
	Reads map[string]string `json:"reads"`
	// Externals records all external reads and negative probes using stat identity.
	Externals     map[string]string   `json:"externals,omitempty"`
	ExternalKinds map[string]ReadKind `json:"external_kinds,omitempty"`
	// Managed 是命令产出的路径，计算目录状态时两侧都要排除它们。
	Managed []string `json:"managed,omitempty"`
	// Writes 是产物，按 Managed 的顺序无关方式回放。
	Writes []Change `json:"writes,omitempty"`

	ExitCode   int   `json:"exit_code"`
	DurationNS int64 `json:"duration_ns"`
	CreatedAt  int64 `json:"created_at"`

	Output string `json:"output"`
}

// Result 是一次命令执行的可复用输出。
type Result struct {
	ExitCode int
	Output   string
	Duration time.Duration
}

// Result 返回条目记录的执行结果。
func (e *Entry) Result() Result {
	return Result{
		ExitCode: e.ExitCode,
		Output:   e.Output,
		Duration: time.Duration(e.DurationNS),
	}
}

// fingerprint 由依赖集唯一决定，用作条目 ID：同一命令在同样的依赖状态下
// 重复执行只会覆盖同一个文件，缓存不会随重跑次数膨胀。
func (e *Entry) fingerprint() string {
	hasher := sha256.New()
	for _, rel := range sortedMapKeys(e.Reads) {
		fmt.Fprintf(hasher, "r\t%s\t%s\n", rel, e.Reads[rel])
	}
	for _, abs := range sortedMapKeys(e.Externals) {
		fmt.Fprintf(hasher, "e\t%s\t%s\n", abs, e.Externals[abs])
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (e *Entry) managedSet() map[string]struct{} {
	if len(e.Managed) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(e.Managed))
	for _, rel := range e.Managed {
		set[rel] = struct{}{}
	}
	return set
}

// matches 校验条目的依赖在 live 树里是否原样成立。
// 只 stat/hash 读集里那几十个路径，不扫全树——这正是相对整树指纹的收益来源。
func (e *Entry) matches(live string, key Key, home ...string) (bool, string) {
	managed := e.managedSet()
	for _, rel := range sortedMapKeys(e.Reads) {
		root, name, err := dependencyRoot(live, rel, home...)
		if err != nil {
			return false, dependencyKind(rel, e.Reads[rel])
		}
		state, err := verifyState(root, name, e.Reads[rel], managed)
		if err != nil {
			// 路径非法或不可读：这条条目不可信，当 miss 处理。
			return false, dependencyKind(rel, e.Reads[rel])
		}
		if state != e.Reads[rel] {
			return false, dependencyKind(rel, e.Reads[rel])
		}
	}
	for _, abs := range sortedMapKeys(e.Externals) {
		want := e.Externals[abs]
		if externalReadState(abs, key, e.ExternalKinds[abs]) != want {
			kind := e.ExternalKinds[abs]
			if want == stateAbsent {
				kind = ReadAbsent
			}
			return false, "external_" + readKindName(kind)
		}
	}
	return true, ""
}

func externalReadState(abs string, key Key, kind ReadKind) string {
	// A process's cwd/root/fds describe that process's workspace. Probing the
	// cache process instead cannot validate the executed child's observation.
	if processWorkspacePath(abs) {
		return ""
	}
	if key.ExternalPath != nil {
		backing := key.ExternalPath(abs)
		if backing == "" {
			if kind == ReadStat && key.ExternalType != nil {
				if typ := key.ExternalType(abs); typ != "" {
					return "v:" + typ
				}
			}
			return stateAbsent
		}
		abs = backing
	}
	return externalState(abs)
}

func processWorkspacePath(abs string) bool {
	parts := strings.Split(strings.TrimPrefix(path.Clean(abs), "/proc/"), "/")
	if !strings.HasPrefix(abs, "/proc/") || len(parts) < 2 {
		return false
	}
	return parts[1] == "cwd" || parts[1] == "root" || parts[1] == "fd" || parts[1] == "fdinfo"
}

func dependencyRoot(live, rel string, home ...string) (string, string, error) {
	if !strings.HasPrefix(rel, "~/") {
		return live, rel, nil
	}
	if len(home) != 1 || home[0] == "" {
		return "", "", fmt.Errorf("cmdcache: HOME required to validate a ~/ dependency")
	}
	return home[0], strings.TrimPrefix(rel, "~/"), nil
}

// externalState includes ctime and inode identity: installers may preserve size
// and mtime when replacing a dependency. Follow symlinks for opened content,
// while retaining the link's own identity for readlink observations.
func externalState(abs string) string {
	var link, target unix.Stat_t
	if err := unix.Lstat(abs, &link); err != nil {
		if absentPath(err) {
			return stateAbsent
		}
		return ""
	}
	state := statIdentity(&link)
	if link.Mode&unix.S_IFMT == unix.S_IFLNK {
		if err := unix.Stat(abs, &target); err != nil {
			if absentPath(err) {
				return state + ":" + stateAbsent
			}
			return ""
		}
		state += ":" + statIdentity(&target)
	}
	return state
}

func statIdentity(st *unix.Stat_t) string {
	return fmt.Sprintf("x:%d:%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Mode, st.Size, st.Mtim.Nano(), st.Ctim.Nano())
}

func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
