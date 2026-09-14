package cmdcache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// stateAbsent 是不存在路径的状态串。负依赖靠它表达。
const stateAbsent = "absent"

// typeStatePrefix 标识只比对类型的元数据状态串。
const typeStatePrefix = "t:"

// pathTypeState 只返回路径的类型，不看内容。
//
// stat 类访问只观测得到元数据，命令根本没读内容，把内容哈希记成依赖会让
// 一切无关编辑都失效。`go build .` 枚举包目录时会 stat 到 README.md，
// 靠这个区分，同事编辑 README 之后构建缓存仍然命中。
func pathTypeState(root, rel string) (string, error) {
	full, err := resolveRel(root, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return stateAbsent, nil
		}
		return "", fmt.Errorf("cmdcache: stat %q: %w", rel, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return typeStatePrefix + "symlink", nil
	case info.IsDir():
		return typeStatePrefix + "dir", nil
	case info.Mode().IsRegular():
		return typeStatePrefix + "file", nil
	default:
		return typeStatePrefix + "other", nil
	}
}

// verifyState 按记录时的状态串形态，用同一种算法重新计算当前状态。
// 状态串的前缀本身就说明了它是怎么算出来的，所以不必另存种类。
func verifyState(root, rel, want string, managed map[string]struct{}) (string, error) {
	if want == stateAbsent || strings.HasPrefix(want, typeStatePrefix) {
		return pathTypeState(root, rel)
	}
	return pathStateExcluding(root, rel, managed)
}

// PathState 返回 live 树里一个路径的状态串。
//
// 读集校验和写集前置条件用的是同一套编码，这样「命中前提」只有一个概念：
// 记录时的状态串和现在的状态串逐条相等。
//
// 编码按类型分开，使得文件、目录、符号链接、不存在四者互不混淆：
//
//	absent            不存在
//	f:<sha256>:<x|->  普通文件，内容摘要 + 可执行位
//	d:<sha256>        目录，条目名与类型的摘要（不含条目内容）
//	l:<sha256>        符号链接，链接目标的摘要
//
// 目录只哈希条目名是有意的：`go build ./...` 依赖的是有哪些包，
// 每个包的内容由各自的文件依赖负责。
func PathState(root, rel string) (string, error) {
	return pathStateExcluding(root, rel, nil)
}

// pathStateExcluding 与 PathState 相同，但计算目录状态时跳过 managed 里的路径。
//
// 这解决一个隐蔽的偏差：命令在自己枚举过的目录里新建文件，会改变该目录的
// 条目集，于是「执行后算出的目录状态」不等于执行前的状态。`go build -o app`
// 记下的 "." 里含 app，全新环境永远匹配不上，第一次缓存就白存了。
//
// 两侧都排除命令管辖的名字即可对齐：记录时和校验时用同一份 managed 集合。
// 这样目录依赖表达的是「除了这条命令自己产出的东西，条目集必须一致」。
// 若命令真的读了某个产物路径的内容，它会同时出现在读集和写集里，
// 被 rewritesOwnInput 判成不可缓存，不会漏进来。
func pathStateExcluding(root, rel string, managed map[string]struct{}) (string, error) {
	full, err := resolveRel(root, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return stateAbsent, nil
		}
		return "", fmt.Errorf("cmdcache: stat %q: %w", rel, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", fmt.Errorf("cmdcache: readlink %q: %w", rel, err)
		}
		sum := sha256.Sum256([]byte(target))
		return "l:" + hex.EncodeToString(sum[:]), nil
	case info.IsDir():
		sum, err := dirDigest(full, rel, managed)
		if err != nil {
			return "", err
		}
		return "d:" + sum, nil
	case info.Mode().IsRegular():
		sum, err := fileDigest(full)
		if err != nil {
			return "", err
		}
		return "f:" + sum + ":" + executableMark(info.Mode()), nil
	default:
		// 设备、FIFO、socket 不是可复用的内容，用类型串占位即可。
		return "o:" + info.Mode().Type().String(), nil
	}
}

func executableMark(mode os.FileMode) string {
	if mode&0o111 != 0 {
		return "x"
	}
	return "-"
}

func fileDigest(full string) (string, error) {
	file, err := os.Open(full)
	if err != nil {
		return "", fmt.Errorf("cmdcache: open %q: %w", full, err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("cmdcache: read %q: %w", full, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func dirDigest(full, rel string, managed map[string]struct{}) (string, error) {
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", fmt.Errorf("cmdcache: read dir %q: %w", full, err)
	}
	base := path.Clean(rel)
	if base == "." {
		base = ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, skip := managed[path.Join(base, entry.Name())]; skip {
			continue
		}
		names = append(names, entry.Name()+"\x00"+entry.Type().String())
	}
	sort.Strings(names)
	hasher := sha256.New()
	for _, name := range names {
		_, _ = io.WriteString(hasher, name+"\n")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// resolveRel 把工作区相对路径解析成绝对路径，并拒绝一切逃逸。
// 缓存条目来自磁盘，回放会照着它写文件；路径校验是这条链路上的边界检查。
func resolveRel(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("cmdcache: empty path")
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("cmdcache: path %q must be relative", rel)
	}
	clean := path.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("cmdcache: path %q escapes the workspace", rel)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}
