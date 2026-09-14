package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveViewLinks follows links in the logical view, so a floor link reads the
// task's version of its target. Caller holds s.mu. Snapshot comparison itself
// uses link nodes, without resolving or reading their targets.
func (s *Store) resolveViewLinks(envID, rel string) (string, error) {
	return resolveLinks(s.floorDir, rel, func(prefix string) (string, bool, error) {
		if b, found := s.lookupBlobValue(envID, prefix); found {
			return string(b.data), !b.tombstone && b.mode&fs.ModeSymlink != 0, nil
		}
		target, link, err := readLinkNode(filepath.Join(s.floorDir, filepath.FromSlash(prefix)))
		// An overlay directory may replace a file in the floor.
		if errors.Is(err, syscall.ENOTDIR) {
			return "", false, nil
		}
		return target, link, err
	})
}

func readLinkNode(name string) (string, bool, error) {
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "", false, nil
	}
	target, err := os.Readlink(name)
	return target, true, err
}

func resolveLinks(root, rel string, lookup func(string) (string, bool, error)) (string, error) {
	remaining := strings.Split(rel, "/")
	var resolved []string
	links := 0
	for len(remaining) > 0 {
		part := remaining[0]
		remaining = remaining[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		prefix := strings.Join(append(append([]string{}, resolved...), part), "/")
		target, isLink, err := lookup(prefix)
		if err != nil {
			return "", err
		}
		if !isLink {
			resolved = append(resolved, part)
			continue
		}
		links++
		if links > 40 {
			return "", fmt.Errorf("vfs: too many symbolic links in %q", rel)
		}
		if filepath.IsAbs(target) {
			if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
				return "", fmt.Errorf("%w: %q", ErrInvalidPath, rel)
			}
			target = strings.TrimPrefix(target, root)
			resolved = nil
		}
		// Expand a link before processing '..' in its target.
		remaining = append(strings.Split(filepath.ToSlash(target), "/"), remaining...)
	}
	if len(resolved) == 0 {
		return ".", nil
	}
	return strings.Join(resolved, "/"), nil
}
