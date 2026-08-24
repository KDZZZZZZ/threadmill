//go:build linux

package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type upperPathKind uint8

const (
	upperDirectory upperPathKind = iota + 1
	upperRegular
	upperWhiteout
)

type upperEntry struct {
	rel      string
	path     string
	info     fs.FileInfo
	whiteout bool
}

type upperScan struct {
	entries []upperEntry
	paths   map[string]upperPathKind
	visited uint64
}

type upperBefore struct {
	snapshot fileSnapshot
	file     bool
	dir      bool
}

// absorbOverlayUpper reads only the active OverlayFS upper layer. Whiteouts
// are the deletion journal; opaque, redirect, metacopy, symlink, or otherwise
// unfamiliar evidence falls back to the merged-tree implementation.
// See https://docs.kernel.org/filesystems/overlayfs.html#whiteouts-and-opaque-directories.
func (s *Store) absorbOverlayUpper(envID, live string) (
	attempted, used bool,
	visited uint64,
	err error,
) {
	upper, ok := s.overlayUpper(envID)
	if !ok {
		return false, false, 0, nil
	}
	ignored := gitIgnoredPaths(live)
	scan, supported, scanErr := scanUpperLayer(upper, ignored)
	if scanErr != nil || !supported {
		return true, false, scan.visited, nil
	}

	s.mu.Lock()
	if current, ok := s.lives[envID]; !ok || current != live {
		s.mu.Unlock()
		return true, true, scan.visited, nil
	}
	before := make(map[string]upperBefore, len(scan.entries))
	for _, entry := range scan.entries {
		before[entry.rel] = s.upperBeforeLocked(envID, entry.rel)
	}
	overlayFiles := s.visibleOverlayFilesLocked(envID)
	s.mu.Unlock()

	for _, entry := range scan.entries {
		if entry.whiteout && before[entry.rel].dir {
			return true, false, scan.visited, nil
		}
	}

	tombstones := make(map[string]blob)
	writes := make(map[string]blob)
	var totalSize int64
	compareA := make([]byte, 32*1024)
	compareB := make([]byte, len(compareA))
	for _, entry := range scan.entries {
		old := before[entry.rel]
		if entry.whiteout {
			if old.file {
				tombstones[entry.rel] = blob{tombstone: true}
			}
			continue
		}
		current, changed, size, readErr := readUpperRegular(entry, old, compareA, compareB)
		if readErr != nil {
			return true, true, scan.visited, readErr
		}
		if !changed {
			continue
		}
		totalSize += size
		if totalSize > MaxTotalSize {
			return true, true, scan.visited, fmt.Errorf("%w: limit %d", ErrTotalSizeExceeded, MaxTotalSize)
		}
		writes[entry.rel] = current
	}
	for rel := range overlayFiles {
		if !upperCovers(scan.paths, rel) {
			tombstones[rel] = blob{tombstone: true}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.lives[envID]; !ok || current != live {
		return true, true, scan.visited, nil
	}
	dst := s.ensure(envID)
	for rel, b := range tombstones {
		applyBlob(dst, rel, b)
	}
	for rel, b := range writes {
		applyBlob(dst, rel, b)
	}
	return true, true, scan.visited, nil
}

func (s *Store) overlayUpper(envID string) (string, bool) {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()
	mount := s.mounts[envID]
	if mount == nil || mount.driver == nil || mount.upperdir == "" {
		return "", false
	}
	if !overlayMounted(mount.mountpoint) {
		return "", false
	}
	return mount.upperdir, true
}

func (s *Store) upperBeforeLocked(envID, rel string) upperBefore {
	if snapshot, ok := s.regularFileSnapshot(envID, rel); ok {
		return upperBefore{snapshot: snapshot, file: true}
	}
	if b, found := s.lookupBlobValue(envID, rel); found && b.tombstone {
		return upperBefore{}
	}
	if s.hasOverlayChildren(envID, rel) {
		return upperBefore{dir: true}
	}
	host, err := s.resolveHost(rel)
	if err != nil {
		return upperBefore{}
	}
	info, err := os.Stat(host)
	return upperBefore{dir: err == nil && info.IsDir()}
}

func (s *Store) visibleOverlayFilesLocked(envID string) map[string]struct{} {
	candidates := make(map[string]struct{})
	for _, files := range s.overlayMaps(envID) {
		for rel := range files {
			candidates[rel] = struct{}{}
		}
	}
	visible := make(map[string]struct{}, len(candidates))
	for rel := range candidates {
		if b, found := s.lookupBlobValue(envID, rel); found && !b.tombstone {
			visible[rel] = struct{}{}
		}
	}
	return visible
}

func readUpperRegular(
	entry upperEntry,
	old upperBefore,
	compareA, compareB []byte,
) (blob, bool, int64, error) {
	executable := entry.info.Mode().Perm()&0o111 != 0
	if old.file && old.snapshot.source != "" && old.snapshot.executable == executable {
		equal, err := equalFileContents(
			entry.path,
			entry.info,
			old.snapshot.source,
			old.snapshot.sourceInfo,
			compareA,
			compareB,
		)
		if err != nil {
			return blob{}, false, 0, err
		}
		if equal {
			return blob{}, false, 0, nil
		}
	}
	if entry.info.Size() > MaxFileSize {
		return blob{}, false, 0, fmt.Errorf(
			"%w: %q (%d > %d)",
			ErrFileTooLarge,
			entry.rel,
			entry.info.Size(),
			MaxFileSize,
		)
	}
	data, err := os.ReadFile(entry.path)
	if err != nil {
		return blob{}, false, 0, err
	}
	size := int64(len(data))
	if size > MaxFileSize {
		return blob{}, false, 0, fmt.Errorf(
			"%w: %q (%d > %d)",
			ErrFileTooLarge,
			entry.rel,
			size,
			MaxFileSize,
		)
	}
	if old.file && old.snapshot.source == "" && old.snapshot.executable == executable && bytes.Equal(old.snapshot.data, data) {
		return blob{}, false, 0, nil
	}
	return blob{data: data, executable: executable}, true, size, nil
}

func scanUpperLayer(root string, ignored map[string]bool) (upperScan, bool, error) {
	scan := upperScan{paths: make(map[string]upperPathKind)}
	if _, supported, err := overlayEntryMetadata(root, nil); err != nil || !supported {
		return scan, supported, err
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(root, path) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errUnsupportedUpperDelta
		}
		whiteout, supported, err := overlayEntryMetadata(path, info)
		if err != nil {
			return err
		}
		if !supported {
			return errUnsupportedUpperDelta
		}
		if directory, skip := ignored[rel]; skip {
			if directory && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		scan.visited++
		switch {
		case info.IsDir():
			scan.paths[rel] = upperDirectory
		case whiteout:
			scan.paths[rel] = upperWhiteout
			scan.entries = append(scan.entries, upperEntry{rel: rel, path: path, info: info, whiteout: true})
		case info.Mode().IsRegular():
			scan.paths[rel] = upperRegular
			scan.entries = append(scan.entries, upperEntry{rel: rel, path: path, info: info})
		default:
			return fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, info.Mode().String())
		}
		return nil
	})
	if errors.Is(err, errUnsupportedUpperDelta) {
		return scan, false, nil
	}
	if err != nil {
		return scan, false, err
	}
	return scan, true, nil
}

var errUnsupportedUpperDelta = errors.New("unsupported OverlayFS upper evidence")

func overlayEntryMetadata(path string, info fs.FileInfo) (whiteout, supported bool, err error) {
	if info != nil && info.Mode()&os.ModeCharDevice != 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		return ok && stat.Rdev == 0, ok && stat.Rdev == 0, nil
	}
	attrs, err := listOverlayXattrs(path)
	if err != nil {
		return false, false, err
	}
	for _, attr := range attrs {
		if attr == "trusted.overlay.whiteout" || attr == "user.overlay.whiteout" {
			if info == nil || !info.Mode().IsRegular() || info.Size() != 0 {
				return false, false, nil
			}
			whiteout = true
			continue
		}
		switch attr {
		case "trusted.overlay.origin", "trusted.overlay.impure", "trusted.overlay.uuid",
			"user.overlay.origin", "user.overlay.impure", "user.overlay.uuid",
			"user.fuseoverlayfs.origin", "user.fuseoverlayfs.impure", "user.fuseoverlayfs.uuid":
			continue
		}
		if strings.HasPrefix(attr, "trusted.overlay.") ||
			strings.HasPrefix(attr, "trusted.overlayfs.") ||
			strings.HasPrefix(attr, "user.overlay.") ||
			strings.HasPrefix(attr, "user.fuseoverlayfs.") {
			return false, false, nil
		}
	}
	return whiteout, true, nil
}

func listOverlayXattrs(path string) ([]string, error) {
	for {
		size, err := syscall.Listxattr(path, nil)
		if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return nil, nil
		}
		if err != nil || size == 0 {
			return nil, err
		}
		buf := make([]byte, size)
		size, err = syscall.Listxattr(path, buf)
		if errors.Is(err, syscall.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var attrs []string
		for _, attr := range bytes.Split(buf[:size], []byte{0}) {
			if len(attr) != 0 {
				attrs = append(attrs, string(attr))
			}
		}
		return attrs, nil
	}
}

func upperCovers(paths map[string]upperPathKind, rel string) bool {
	if kind, ok := paths[rel]; ok {
		return kind != upperDirectory
	}
	for _, ancestor := range ancestorPrefixes(rel) {
		if kind, ok := paths[ancestor]; ok && kind != upperDirectory {
			return true
		}
	}
	return false
}
