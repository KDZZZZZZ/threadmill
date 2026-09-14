package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const liveFingerprintBuckets = 256

type liveFingerprint struct {
	hash    string
	buckets [liveFingerprintBuckets][sha256.Size]byte
	valid   bool
}

// scanLiveFingerprint compares live file metadata and change identity. Linux
// ctime catches same-size writes even when mtime is restored; other platforms
// fall back to file contents when no reliable change identity is available.
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
