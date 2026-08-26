package cmdcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"
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
}

func (k Key) index() string {
	sum := sha256.Sum256([]byte("tmcmd1\n" + k.Command + "\n" + k.Backend + "\n" + k.EnvHash))
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

	Command string `json:"command"`
	Backend string `json:"backend"`
	EnvHash string `json:"env_hash"`

	// Reads 是推断出的依赖：工作区相对路径 → 执行前状态串。
	Reads map[string]string `json:"reads"`
	// Externals 是工作区之外执行过的二进制：绝对路径 → (size, mtime)。
	// 宿主工具链升级要能让缓存失效。
	Externals map[string]string `json:"externals,omitempty"`
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
func (e *Entry) matches(live string) (bool, error) {
	managed := e.managedSet()
	for _, rel := range sortedMapKeys(e.Reads) {
		state, err := verifyState(live, rel, e.Reads[rel], managed)
		if err != nil {
			// 路径非法或不可读：这条条目不可信，当 miss 处理。
			return false, nil //nolint:nilerr // 校验失败一律保守判 miss
		}
		if state != e.Reads[rel] {
			return false, nil
		}
	}
	for abs, want := range e.Externals {
		if externalState(abs) != want {
			return false, nil
		}
	}
	return true, nil
}

// externalState 用 (size, mtime) 而不是内容摘要标识宿主工具链。
// `go` 二进制上百 MB，每次校验都读一遍会把缓存的收益吃光；
// 工具链升级必然改动这两个值。
func externalState(abs string) string {
	info, err := os.Stat(abs)
	if err != nil {
		return stateAbsent
	}
	return strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
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
