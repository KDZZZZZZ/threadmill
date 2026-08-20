package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
)

// Materialize 把 env 的可见树落到 live 目录。已物化则原样返回。
func (s *Store) Materialize(envID string) (string, error) {
	s.mu.Lock()
	if dir, ok := s.lives[envID]; ok {
		s.mu.Unlock()
		return dir, nil
	}
	base, err := confinedRoot(s.baseDir)
	if err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("vfs: materialize: %w", err)
	}
	blobs := s.overlayBlobs(envID)
	s.mu.Unlock()

	var live string
	if s.liveRoot == "" {
		live, err = os.MkdirTemp("", "threadmill-live-")
	} else {
		if restored, ok, restoreErr := s.persistedLive(envID); restoreErr != nil {
			return "", restoreErr
		} else if ok {
			s.mu.Lock()
			s.lives[envID] = restored
			s.mu.Unlock()
			return restored, nil
		}
		live, err = os.MkdirTemp(s.liveRoot, ".tmp-")
	}
	if err != nil {
		return "", fmt.Errorf("vfs: materialize: %w", err)
	}
	if err := copyTree(base, live); err != nil {
		os.RemoveAll(live)
		return "", err
	}
	for _, item := range blobs {
		if err := applyLive(live, item.path, item.b); err != nil {
			os.RemoveAll(live)
			return "", err
		}
	}
	if s.liveRoot != "" {
		dest := s.persistentLivePath(envID)
		if err := os.Rename(live, dest); err != nil {
			if restored, ok, restoreErr := s.persistedLive(envID); restoreErr == nil && ok {
				_ = os.RemoveAll(live)
				live = restored
			} else {
				_ = os.RemoveAll(live)
				return "", fmt.Errorf("vfs: commit persistent environment: %w", err)
			}
		} else {
			live = dest
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.lives[envID]; ok {
		os.RemoveAll(live)
		return dir, nil
	}
	s.lives[envID] = live
	return live, nil
}

// Absorb 把 live 相对 overlay+host 的增量写回 overlay。未物化或 envID 为空则是空操作。
func (s *Store) Absorb(envID string) error {
	if envID == "" {
		return nil
	}
	s.mu.Lock()
	live, ok := s.lives[envID]
	s.mu.Unlock()
	if !ok {
		return nil
	}

	liveFiles, err := walkRegularFiles(live)
	if err != nil {
		return fmt.Errorf("vfs: absorb: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lives[envID]; !ok {
		return nil
	}
	before, err := s.visibleRegularFiles(envID)
	if err != nil {
		return fmt.Errorf("vfs: absorb: %w", err)
	}
	dst := s.ensure(envID)
	for path := range before {
		if _, ok := liveFiles[path]; !ok {
			applyBlob(dst, path, blob{tombstone: true})
		}
	}
	for path, data := range liveFiles {
		old, existed := before[path]
		if existed && bytes.Equal(old, data) {
			continue
		}
		applyBlob(dst, path, blob{data: data})
	}
	return nil
}

// Release 先把 live 收进 overlay，再删掉 live 目录。未物化则是空操作。
func (s *Store) Release(envID string) error {
	aerr := s.Absorb(envID)
	if s.liveRoot != "" {
		return aerr
	}
	s.mu.Lock()
	live, ok := s.lives[envID]
	if ok {
		delete(s.lives, envID)
	}
	s.mu.Unlock()
	if !ok {
		return aerr
	}
	if err := os.RemoveAll(live); err != nil {
		return errors.Join(aerr, fmt.Errorf("vfs: release: %w", err))
	}
	return aerr
}

// Discard removes a durable environment after its task tree completed.
// In-memory stores keep their existing inspection behavior.
func (s *Store) Discard(envID string) error {
	if s.liveRoot == "" || envID == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.lives, envID)
	delete(s.envs, envID)
	s.mu.Unlock()
	path := s.persistentLivePath(envID)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("vfs: discard persistent environment: %w", err)
	}
	return nil
}

type overlayFile struct {
	path string
	b    blob
}

func (s *Store) overlayBlobs(envID string) []overlayFile {
	maps := s.overlayMaps(envID)
	var out []overlayFile
	for i := len(maps) - 1; i >= 0; i-- {
		var tombs, writes []overlayFile
		for path, b := range maps[i] {
			item := overlayFile{path: path, b: cloneBlob(b)}
			if b.tombstone {
				tombs = append(tombs, item)
				continue
			}
			writes = append(writes, item)
		}
		out = append(out, tombs...)
		out = append(out, writes...)
	}
	return out
}

// walkRegularFiles 扫 live 树并读出每个文件的内容。
// overlay 只存路径到字节（和删除标记），不跟踪 live inode。Absorb 用这份
// 内容和 overlay+host 的可见树做比对，只把增删改写回 overlay，所以必须读字节，
// 不能只看文件名。
// 仅允许常规普通文件；发现 FIFO、socket、设备等特殊文件或文件/总大小超限时返回错误。
func walkRegularFiles(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
	var totalSize int64

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(root, path) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		mode := d.Type()
		if mode&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		// 校验是否为普通文件（非 FIFO、Socket、Device 等特殊文件）
		if mode.Type() != 0 {
			return fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, mode.String())
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileSize {
			return fmt.Errorf("%w: %q (%d > %d)", ErrFileTooLarge, rel, info.Size(), MaxFileSize)
		}
		totalSize += info.Size()
		if totalSize > MaxTotalSize {
			return fmt.Errorf("%w: limit %d", ErrTotalSizeExceeded, MaxTotalSize)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = cloneBytes(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) visibleRegularFiles(envID string) (map[string][]byte, error) {
	candidates := map[string]struct{}{}
	base, err := confinedRoot(s.baseDir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(base, path) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		mode := d.Type()
		if mode&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		if mode.Type() != 0 {
			return fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, mode.String())
		}
		candidates[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, files := range s.overlayMaps(envID) {
		for path := range files {
			candidates[path] = struct{}{}
		}
	}
	out := make(map[string][]byte, len(candidates))
	for path := range candidates {
		data, ok := s.regularFileContent(envID, path)
		if ok {
			out[path] = data
		}
	}
	return out, nil
}

func (s *Store) regularFileContent(envID, rel string) ([]byte, bool) {
	data, tombstone, found := s.lookupBlob(envID, rel)
	if found {
		if tombstone {
			return nil, false
		}
		return cloneBytes(data), true
	}
	if s.hasOverlayChildren(envID, rel) {
		return nil, false
	}
	host, err := s.resolveHost(rel)
	if err != nil {
		return nil, false
	}
	fi, err := os.Stat(host)
	if err != nil || fi.IsDir() || fi.Mode().Type() != 0 || fi.Size() > MaxFileSize {
		return nil, false
	}
	b, err := os.ReadFile(host)
	if err != nil {
		return nil, false
	}
	return b, true
}

func applyLive(live, rel string, b blob) error {
	if b.tombstone {
		return deleteLive(live, rel)
	}
	return writeLive(live, rel, b.data)
}

func readLive(live, rel string) ([]byte, error) {
	dest, err := resolveLive(live, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		return nil, mapIOError("read", rel, err)
	}
	if fi.Mode().Type() != 0 {
		return nil, fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, fi.Mode().Type().String())
	}
	if fi.Size() > MaxFileSize {
		return nil, fmt.Errorf("%w: %q (%d > %d)", ErrFileTooLarge, rel, fi.Size(), MaxFileSize)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return nil, mapIOError("read", rel, err)
	}
	return cloneBytes(data), nil
}

func writeLive(live, rel string, data []byte) error {
	dest, err := createLivePath(live, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o640)
}

func deleteLive(live, rel string) error {
	root, err := confinedRoot(live)
	if err != nil {
		return err
	}
	dest, err := liveCandidate(root, rel)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(dest)
	}
	resolved, err := resolveLive(live, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(resolved); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func statLive(live, rel string) (FileInfo, error) {
	dest, err := resolveLive(live, rel)
	if err != nil {
		return FileInfo{}, err
	}
	fi, err := os.Stat(dest)
	if err != nil {
		return FileInfo{}, mapIOError("stat", rel, err)
	}
	return FileInfo{Name: fi.Name(), Size: fi.Size(), IsDir: fi.IsDir()}, nil
}

func listLive(live, rel string) ([]DirEnt, error) {
	dest, err := resolveLive(live, rel)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(dest)
	if err != nil {
		return nil, mapIOError("list", rel, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("vfs: %s: not a directory", rel)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return nil, mapIOError("list", rel, err)
	}
	ents := make(map[string]DirEnt, len(entries))
	for _, e := range entries {
		ents[e.Name()] = DirEnt{Name: e.Name(), IsDir: e.IsDir()}
	}
	return sortedDirents(ents), nil
}

func resolveLive(live, rel string) (string, error) {
	root, err := confinedRoot(live)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return root, nil
	}
	candidate, err := liveCandidate(root, rel)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			if escapesRoot(root, candidate) {
				return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
			}
			return "", notFound(rel)
		}
		return "", err
	}
	if escapesRoot(root, resolved) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return resolved, nil
}

func createLivePath(live, rel string) (string, error) {
	root, err := confinedRoot(live)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	cur := root
	for _, part := range parts[:len(parts)-1] {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		next := filepath.Join(cur, part)
		if escapesRoot(root, next) {
			return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		fi, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				cur = next
				continue
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", err
			}
			if escapesRoot(root, resolved) {
				return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
			}
			cur = resolved
			continue
		}
		cur = next
	}
	base := parts[len(parts)-1]
	if base == "." || base == ".." {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	dest := filepath.Join(cur, base)
	if escapesRoot(root, dest) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return dest, nil
}

func liveCandidate(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if escapesRoot(root, full) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	return full, nil
}

func copyTree(src, dst string) error {
	cmd := osexec.Command("cp", "--reflink=auto", "-a", src+"/.", dst)
	if err := cmd.Run(); err == nil {
		return nil
	}
	return copyWalk(src, dst)
}

func copyWalk(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		target := filepath.Join(dst, rel)
		if escapesRoot(dst, target) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o640)
	})
}
