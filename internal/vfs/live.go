package vfs

import (
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

// Absorb 把 live 目录里的普通文件写回 overlay。未物化则是空操作。
func (s *Store) Absorb(envID string) error {
	s.mu.Lock()
	live, ok := s.lives[envID]
	s.mu.Unlock()
	if !ok {
		return nil
	}

	type file struct {
		rel  string
		data []byte
	}
	var files []file
	err := filepath.WalkDir(live, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(live, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(live, path) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, file{
			rel:  filepath.ToSlash(rel),
			data: cloneBytes(data),
		})
		return nil
	})
	if err != nil {
		return fmt.Errorf("vfs: absorb: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lives[envID]; !ok {
		return nil
	}
	dst := s.ensure(envID)
	for _, f := range files {
		dst.files[f.rel] = blob{data: f.data}
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
	merged := map[string]blob{}
	chain := s.chain(envID)
	for i := len(chain) - 1; i >= 0; i-- {
		for path, b := range chain[i].files {
			merged[path] = cloneBlob(b)
		}
	}
	out := make([]overlayFile, 0, len(merged))
	for path, b := range merged {
		out = append(out, overlayFile{path: path, b: b})
	}
	return out
}

func applyLive(live, rel string, b blob) error {
	dest, err := livePath(live, rel)
	if err != nil {
		return err
	}
	if b.tombstone {
		if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, b.data, 0o640)
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
