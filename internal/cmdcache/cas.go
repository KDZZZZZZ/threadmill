package cmdcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// blobStore 是产物内容的内容寻址存储。多个条目共享同一份内容：
// 同一个二进制被上百次构建产出时只落一份。
type blobStore struct {
	root string
}

func (s blobStore) path(digest string) string {
	return filepath.Join(s.root, digest[:2], digest)
}

// putFile 把 live 树里的一个文件收进 CAS，返回内容摘要。
func (s blobStore) putFile(source string) (string, error) {
	file, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("cmdcache: open artifact %q: %w", source, err)
	}
	defer file.Close()

	temp, err := os.CreateTemp(s.root, ".tmp-")
	if err != nil {
		return "", fmt.Errorf("cmdcache: stage artifact: %w", err)
	}
	staged := temp.Name()
	defer os.Remove(staged)

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temp, hasher), file); err != nil {
		temp.Close()
		return "", fmt.Errorf("cmdcache: stage artifact %q: %w", source, err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("cmdcache: stage artifact %q: %w", source, err)
	}
	digest := hex.EncodeToString(hasher.Sum(nil))

	dest := s.path(digest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", fmt.Errorf("cmdcache: create blob directory: %w", err)
	}
	// 同名即同内容，已存在就不必再写；rename 保证别的进程要么看到完整内容，
	// 要么什么都看不到。
	if _, err := os.Stat(dest); err == nil {
		return digest, nil
	}
	if err := os.Rename(staged, dest); err != nil {
		return "", fmt.Errorf("cmdcache: commit blob: %w", err)
	}
	return digest, nil
}

// open 打开一份产物内容。缺失说明它被 GC 回收了，调用方按 miss 处理。
func (s blobStore) open(digest string) (*os.File, error) {
	return os.Open(s.path(digest))
}

func (s blobStore) has(digest string) bool {
	_, err := os.Stat(s.path(digest))
	return err == nil
}

type blobInfo struct {
	path    string
	size    int64
	modTime int64
}

// collect 列出所有产物内容及其大小与最近使用时间，供 GC 裁剪。
func (s blobStore) collect() ([]blobInfo, int64, error) {
	var blobs []blobInfo
	var total int64
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		blobs = append(blobs, blobInfo{path: path, size: info.Size(), modTime: info.ModTime().UnixNano()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("cmdcache: scan blobs: %w", err)
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].modTime < blobs[j].modTime })
	return blobs, total, nil
}
