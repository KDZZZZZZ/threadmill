package vfs

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
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

	live, err := os.MkdirTemp("", "threadmill-live-")
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.lives[envID]; ok {
		os.RemoveAll(live)
		return dir, nil
	}
	s.lives[envID] = live
	return live, nil
}

// Absorb 把 live 相对 overlay+host 的增量写回 overlay。未物化则是空操作。
func (s *Store) Absorb(envID string) error {
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

// Release 删掉 live 目录。未物化则是空操作。
func (s *Store) Release(envID string) error {
	s.mu.Lock()
	live, ok := s.lives[envID]
	if ok {
		delete(s.lives, envID)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if err := os.RemoveAll(live); err != nil {
		return fmt.Errorf("vfs: release: %w", err)
	}
	return nil
}

type overlayFile struct {
	path string
	b    blob
}

func (s *Store) overlayBlobs(envID string) []overlayFile {
	chain := s.chain(envID)
	var out []overlayFile
	for i := len(chain) - 1; i >= 0; i-- {
		var tombs, writes []overlayFile
		for path, b := range chain[i].files {
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

func walkRegularFiles(root string) (map[string][]byte, error) {
	out := map[string][]byte{}
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
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
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
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		candidates[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, l := range s.chain(envID) {
		for path := range l.files {
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
	if err != nil || fi.IsDir() {
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
		dest, err := livePath(live, rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeLive(live, rel, b.data)
}

func liveDest(live, rel string) (string, error) {
	if rel == "." {
		return live, nil
	}
	return livePath(live, rel)
}

func readLive(live, rel string) ([]byte, error) {
	dest, err := liveDest(live, rel)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		return nil, mapIOError("read", rel, err)
	}
	return cloneBytes(data), nil
}

func writeLive(live, rel string, data []byte) error {
	dest, err := livePath(live, rel)
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
	dest, err := livePath(live, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func statLive(live, rel string) (FileInfo, error) {
	dest, err := liveDest(live, rel)
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
	dest, err := liveDest(live, rel)
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

func livePath(live, rel string) (string, error) {
	if rel == "" || rel == "." {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	if filepath.IsAbs(rel) || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	full := filepath.Join(live, filepath.FromSlash(rel))
	if escapesRoot(live, full) {
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
