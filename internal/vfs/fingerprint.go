package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const liveFingerprintBuckets = 256

type liveFingerprint struct {
	hash    string
	buckets [liveFingerprintBuckets][sha256.Size]byte
	valid   bool
}

// StateFingerprint 唯一标识一个环境的可见状态：
// 基线仓纪元（每 Store 一次）⊕ overlay 链内容 ⊕ live 树 stat 向量。
// 命令结果缓存以 (fingerprint, command) 为键；指纹变化即视为输入变化。
// 已知限制：进程存活期间宿主基线被外部编辑不会反映到纪元里（设计上基线只读）。
func (s *Store) StateFingerprint(envID string) string {
	s.mu.Lock()
	overlay := s.overlayFingerprintLocked(envID)
	s.mu.Unlock()
	live := s.LiveStatHash(envID)
	base := s.baseEpoch()
	sum := sha256.Sum256([]byte("tm1\n" + base + "\n" + overlay + "\n" + live))
	return hex.EncodeToString(sum[:])
}

// LiveStatHash 返回 live 树的 stat 向量哈希；未物化返回空串。
// 命令执行前后各取一次，相同即证明命令没有改动 live 树。
// Linux 使用 device、inode 和 ctime 捕获同尺寸、同 mtime 的原地写入；
// 没有可靠 change identity 的平台会退回到普通文件内容摘要。
// LiveStatHash 只在取 live 路径时短暂持锁，目录遍历在锁外进行，
// 避免数千文件的 stat 扫描串行化其他环境的读写。
func (s *Store) LiveStatHash(envID string) string {
	s.mu.Lock()
	live, ok := s.lives[envID]
	s.mu.Unlock()
	if !ok {
		return ""
	}
	return scanLiveFingerprint(live).hash
}

func scanLiveFingerprint(live string) *liveFingerprint {
	fingerprint := &liveFingerprint{valid: true}
	hasher := sha256.New()
	err := filepath.WalkDir(live, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(live, path)
		if relErr != nil || rel == "." {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		entry := sha256.New()
		fmt.Fprintf(
			entry,
			"%s %s %d %d %o",
			info.Mode().Type().String(),
			filepath.ToSlash(rel),
			info.Size(),
			info.ModTime().UnixNano(),
			info.Mode().Perm(),
		)
		if identity, ok := statChangeIdentity(info); ok {
			fmt.Fprintf(
				entry,
				" %d %d %d %d\n",
				identity.device,
				identity.inode,
				identity.seconds,
				identity.nanoseconds,
			)
		} else if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			fileSum := sha256.Sum256(data)
			fmt.Fprintf(entry, " %s\n", hex.EncodeToString(fileSum[:]))
		} else {
			fmt.Fprintln(entry)
		}
		entrySum := entry.Sum(nil)
		_, _ = hasher.Write(entrySum)
		if info.Mode().IsRegular() {
			bucket := liveFingerprintBucket(filepath.ToSlash(rel))
			for i, value := range entrySum {
				fingerprint.buckets[bucket][i] ^= value
			}
		}
		return nil
	})
	if err != nil {
		// 无法枚举的 live 树指纹置为一次性随机值，保守视为状态已变。
		fingerprint.hash = fmt.Sprintf("err-%v-%d", err, time.Now().UnixNano())
		fingerprint.valid = false
		return fingerprint
	}
	fingerprint.hash = hex.EncodeToString(hasher.Sum(nil))
	return fingerprint
}

func liveFingerprintBucket(path string) uint8 {
	var hash uint32 = 2166136261
	for i := range len(path) {
		hash ^= uint32(path[i])
		hash *= 16777619
	}
	return uint8(hash)
}

func unchangedLiveBuckets(before, after *liveFingerprint) [liveFingerprintBuckets]bool {
	var unchanged [liveFingerprintBuckets]bool
	if before == nil || after == nil || !before.valid || !after.valid {
		return unchanged
	}
	for i := range unchanged {
		unchanged[i] = before.buckets[i] == after.buckets[i]
	}
	return unchanged
}

// overlayFingerprintLocked 对 overlay 链（含冻结基线）做内容哈希；
// 同一路径只取链上最上层值，与 lookup 语义一致。调用方持锁。
func (s *Store) overlayFingerprintLocked(envID string) string {
	maps := s.overlayMaps(envID)
	hasher := sha256.New()
	seen := make(map[string]blob, 64)
	for _, files := range maps {
		for path, b := range files {
			if _, dup := seen[path]; dup {
				continue
			}
			seen[path] = b
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b := seen[path]
		if b.tombstone {
			fmt.Fprintf(hasher, "%s\tT\n", path)
			continue
		}
		blobSum := sha256.Sum256(b.data)
		fmt.Fprintf(hasher, "%s\t%d\t%s\n", path, boolToInt(b.executable), hex.EncodeToString(blobSum[:]))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// baseEpoch 返回本 Store 基线仓的一次性 stat 纪元；只在首次调用时扫描。
func (s *Store) baseEpoch() string {
	s.epochMu.Lock()
	defer s.epochMu.Unlock()
	if s.epoch != "" {
		return s.epoch
	}
	hasher := sha256.New()
	if root, err := confinedRoot(s.baseDir); err == nil {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // 基线纪元取尽力而为的扫描
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil || rel == "." {
				return nil
			}
			info, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			fmt.Fprintf(hasher, "%s|%d|%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
			return nil
		})
	}
	s.epoch = hex.EncodeToString(hasher.Sum(nil))
	return s.epoch
}
