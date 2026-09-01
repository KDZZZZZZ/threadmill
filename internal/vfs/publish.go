package vfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	publishedPathsName = ".published.json"
	replacedDirName    = ".replaced"
	retainedReplaced   = 10
)

// PublishReceipt reports what a publication changed on the display surface. It
// is the only account of what the user can now see, so callers describe a
// publication from it rather than from the fact that it succeeded.
type PublishReceipt struct {
	Added    []string `json:"added,omitempty"`
	Updated  []string `json:"updated,omitempty"`
	Deleted  []string `json:"deleted,omitempty"`
	Replaced string   `json:"replaced,omitempty"`
}

// Changed reports how many display paths the publication touched. Zero is a
// legitimate outcome — a checkpoint that matches what is already displayed —
// and callers must not report it as a delivery.
func (r PublishReceipt) Changed() int {
	return len(r.Added) + len(r.Updated) + len(r.Deleted)
}

// Publish renders envID's visible files onto the display surface so the user
// can see that checkpoint in their own project directory.
//
// Publication is a display operation, not a commit: it never touches the read
// floor, never invalidates or discards an environment, and so needs no
// quiescent graph. Any completed checkpoint may be rendered at any time, and an
// earlier one may be rendered again to go back.
//
// It reconciles rather than rewrites. Only paths the checkpoint overrides and
// paths an earlier publication added are considered, and within those only ones
// whose content actually differs are written, so an interrupted publication is
// repaired by the next one instead of needing a transaction. Paths that exist
// only on the display surface — files the user or a build created after the
// session adopted the project — are never touched, and .git never is either.
//
// Content that a publication overwrites or removes is saved under the store's
// replaced directory first, so an edit made directly in the project directory
// survives being displaced by a checkpoint.
func (s *Store) Publish(envID string) (receipt PublishReceipt, retErr error) {
	if envID == "" {
		return PublishReceipt{}, nil
	}
	started := time.Now()
	committed := false
	s.mu.Lock()
	s.publishAttempts++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.publishDuration += time.Since(started)
		if committed {
			s.publishCommits++
		}
		if retErr != nil {
			s.publishErrors++
		}
		s.mu.Unlock()
	}()

	// Absorb, not Freeze: a display operation must not tear down the workspace
	// it is rendering from.
	if err := s.Absorb(envID); err != nil {
		return PublishReceipt{}, err
	}
	display, err := confinedRoot(s.displayDir)
	if err != nil {
		return PublishReceipt{}, fmt.Errorf("vfs: publish: %w", err)
	}

	published, err := s.loadPublishedPaths()
	if err != nil {
		return PublishReceipt{}, err
	}

	s.mu.Lock()
	blobs := s.overlayBlobs(envID)
	s.mu.Unlock()
	candidates := make(map[string]struct{}, len(blobs)+len(published))
	for _, item := range blobs {
		candidates[filepath.ToSlash(item.path)] = struct{}{}
	}
	for path := range published {
		candidates[path] = struct{}{}
	}
	paths := make([]string, 0, len(candidates))
	for path := range candidates {
		if path == ".git" || strings.HasPrefix(path, ".git/") {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)

	plan, err := s.planPublish(envID, display, paths, published)
	if err != nil {
		return PublishReceipt{}, err
	}
	if len(plan) == 0 {
		committed = true
		return PublishReceipt{}, nil
	}

	receipt, err = s.applyPublish(display, plan, published)
	if err != nil {
		return receipt, err
	}
	committed = true

	if err := s.savePublishedPaths(published); err != nil {
		return receipt, err
	}
	// Record the display state this publication produced. Without it the next
	// session would see a project that no longer matches the floor, re-adopt it,
	// and discard the very checkpoints it should be resuming from.
	if err := s.recordDisplayState(); err != nil {
		return receipt, err
	}
	// When floor and display are the same directory the publication did change
	// what environments read through, so the cached listing has to go.
	if s.displayDir == s.floorDir {
		s.invalidateFloorCache()
	}
	return receipt, nil
}

type publishAction uint8

const (
	publishWrite publishAction = iota
	publishRemove
	publishRemoveDir
)

type publishStep struct {
	path       string
	action     publishAction
	data       []byte
	executable bool
	existed    bool
}

// planPublish decides what each candidate path needs, reading but not writing.
//
// Tracked paths are the project's own files — the ones the floor holds — plus
// whatever earlier publications added. Only those may be removed; anything else
// on the display surface belongs to the user.
func (s *Store) planPublish(
	envID, display string,
	paths []string,
	published map[string]struct{},
) ([]publishStep, error) {
	s.mu.Lock()
	floorFiles, err := s.cachedBaseRegularFiles()
	floorDirs := s.baseDirs
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("vfs: publish read floor: %w", err)
	}
	tracked := make(map[string]struct{}, len(floorFiles)+len(published))
	for path := range floorFiles {
		tracked[path] = struct{}{}
	}
	for path := range published {
		tracked[path] = struct{}{}
	}

	files, directories := expandPublishCandidates(paths, display, tracked, floorDirs)
	steps := make([]publishStep, 0, len(files)+len(directories))
	for _, path := range files {
		target, err := publishTargetPath(display, path)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		want, visible := s.regularFileSnapshot(envID, path)
		s.mu.Unlock()

		have, err := readDisplayFile(target)
		if err != nil {
			return nil, err
		}
		if !visible {
			if _, ours := tracked[path]; have.exists && ours {
				steps = append(steps, publishStep{path: path, action: publishRemove, existed: true})
			}
			continue
		}
		data, err := publishContent(want)
		if err != nil {
			return nil, err
		}
		if have.exists && have.regular &&
			have.executable == want.executable && bytes.Equal(have.data, data) {
			continue
		}
		steps = append(steps, publishStep{
			path:       path,
			action:     publishWrite,
			data:       data,
			executable: want.executable,
			existed:    have.exists,
		})
	}
	// Directories go last and deepest first, so a directory is only reclaimed
	// once whatever the checkpoint dropped inside it is gone.
	for _, path := range directories {
		steps = append(steps, publishStep{path: path, action: publishRemoveDir})
	}
	return steps, nil
}

// expandPublishCandidates splits candidates into file paths and directories to
// reclaim. A checkpoint records a removed directory as a single tombstone, but a
// directory can only leave the display surface by having its tracked contents
// leave first — a build output or an untracked file inside it keeps it alive.
func expandPublishCandidates(
	paths []string,
	display string,
	tracked map[string]struct{},
	floorDirs map[string]struct{},
) (files, directories []string) {
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range paths {
		target, err := publishTargetPath(display, path)
		if err != nil {
			continue
		}
		info, err := os.Lstat(target)
		if err != nil || !info.IsDir() {
			continue
		}
		prefix := path + "/"
		for candidate := range tracked {
			if strings.HasPrefix(candidate, prefix) {
				seen[candidate] = struct{}{}
			}
		}
		_, fromFloor := floorDirs[path]
		if _, ours := tracked[path]; fromFloor || ours {
			directories = append(directories, path)
		}
	}
	for path := range seen {
		files = append(files, path)
	}
	slices.Sort(files)
	slices.Sort(directories)
	slices.Reverse(directories)
	return files, directories
}

func (s *Store) applyPublish(
	display string,
	plan []publishStep,
	published map[string]struct{},
) (PublishReceipt, error) {
	var receipt PublishReceipt
	replaced, err := s.newReplacedDir()
	if err != nil {
		return receipt, err
	}
	saved := false
	for _, step := range plan {
		target, err := publishTargetPath(display, step.path)
		if err != nil {
			return receipt, err
		}
		if step.existed && step.action != publishRemoveDir && replaced != "" {
			kept, err := retainReplaced(replaced, step.path, target)
			if err != nil {
				return receipt, err
			}
			saved = saved || kept
		}
		switch step.action {
		case publishWrite:
			if err := writeDisplayFile(target, step.data, step.executable); err != nil {
				return receipt, err
			}
			published[step.path] = struct{}{}
			if step.existed {
				receipt.Updated = append(receipt.Updated, step.path)
			} else {
				receipt.Added = append(receipt.Added, step.path)
			}
		case publishRemove:
			if err := os.RemoveAll(target); err != nil {
				return receipt, fmt.Errorf("vfs: publish remove %q: %w", step.path, err)
			}
			pruneEmptyDirs(display, filepath.Dir(target))
			delete(published, step.path)
			receipt.Deleted = append(receipt.Deleted, step.path)
		case publishRemoveDir:
			// os.Remove refuses a directory that still holds anything, which is
			// exactly the guard wanted: whatever is left is not ours.
			if err := os.Remove(target); err != nil {
				continue
			}
			pruneEmptyDirs(display, filepath.Dir(target))
			delete(published, step.path)
			receipt.Deleted = append(receipt.Deleted, step.path)
		}
	}
	if replaced != "" {
		if saved {
			receipt.Replaced = replaced
			s.pruneReplaced()
		} else if err := os.RemoveAll(replaced); err != nil {
			s.mu.Lock()
			s.publishCleanupErrors++
			s.mu.Unlock()
		}
	}
	return receipt, nil
}

// floorHasPath reports whether the read floor shows path as a regular file, and
// therefore whether it was part of the project the session adopted.
func (s *Store) floorHasPath(path string) bool {
	host, err := s.resolveHost(path)
	if err != nil {
		return false
	}
	info, err := os.Stat(host)
	return err == nil && info.Mode().IsRegular()
}

func publishContent(want fileSnapshot) ([]byte, error) {
	if want.source == "" {
		return want.data, nil
	}
	data, err := os.ReadFile(want.source)
	if err != nil {
		return nil, fmt.Errorf("vfs: publish read floor: %w", err)
	}
	return data, nil
}

type displayFile struct {
	exists     bool
	regular    bool
	executable bool
	data       []byte
}

func readDisplayFile(target string) (displayFile, error) {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return displayFile{}, nil
		}
		return displayFile{}, fmt.Errorf("vfs: publish inspect %q: %w", target, err)
	}
	if !info.Mode().IsRegular() {
		return displayFile{exists: true}, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return displayFile{}, fmt.Errorf("vfs: publish read %q: %w", target, err)
	}
	return displayFile{
		exists:     true,
		regular:    true,
		executable: info.Mode().Perm()&0o111 != 0,
		data:       data,
	}, nil
}

// writeDisplayFile installs data at target through a sibling temporary file, so
// a reader never observes a partially written path.
func writeDisplayFile(target string, data []byte, executable bool) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("vfs: publish create parent of %q: %w", target, err)
	}
	if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("vfs: publish clear %q: %w", target, err)
		}
	}
	perm := os.FileMode(0o644)
	if executable {
		perm = 0o755
	}
	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tm-")
	if err != nil {
		return fmt.Errorf("vfs: publish stage %q: %w", target, err)
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		return errors.Join(
			fmt.Errorf("vfs: publish write %q: %w", target, err),
			temp.Close(),
			os.Remove(name),
		)
	}
	if err := temp.Close(); err != nil {
		return errors.Join(fmt.Errorf("vfs: publish close %q: %w", target, err), os.Remove(name))
	}
	if err := os.Chmod(name, perm); err != nil {
		return errors.Join(fmt.Errorf("vfs: publish chmod %q: %w", target, err), os.Remove(name))
	}
	if err := os.Rename(name, target); err != nil {
		return errors.Join(fmt.Errorf("vfs: publish install %q: %w", target, err), os.Remove(name))
	}
	return nil
}

// pruneEmptyDirs removes directories left empty by a removal, stopping at the
// display root and at the first directory that still holds something.
func pruneEmptyDirs(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func publishTargetPath(display, path string) (string, error) {
	rel := filepath.FromSlash(path)
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("vfs: publish %q: %w", path, ErrInvalidPath)
	}
	return filepath.Join(display, rel), nil
}

func (s *Store) newReplacedDir() (string, error) {
	if s.liveRoot == "" {
		return "", nil
	}
	root := filepath.Join(s.liveRoot, replacedDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("vfs: create replaced root: %w", err)
	}
	dir := filepath.Join(root, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("vfs: create replaced directory: %w", err)
	}
	return dir, nil
}

// retainReplaced copies the content a publication is about to displace. It
// reports whether anything was kept.
func retainReplaced(replaced, path, target string) (bool, error) {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("vfs: retain replaced %q: %w", path, err)
	}
	dest := filepath.Join(replaced, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return false, fmt.Errorf("vfs: retain replaced %q: %w", path, err)
	}
	if info.IsDir() {
		if err := os.Mkdir(dest, 0o700); err != nil {
			return false, fmt.Errorf("vfs: retain replaced %q: %w", path, err)
		}
		if _, err := copyTree(target, dest); err != nil {
			return false, fmt.Errorf("vfs: retain replaced %q: %w", path, err)
		}
		return true, nil
	}
	if err := copyPublishedPath(target, dest); err != nil {
		return false, fmt.Errorf("vfs: retain replaced %q: %w", path, err)
	}
	return true, nil
}

func (s *Store) pruneReplaced() {
	root := filepath.Join(s.liveRoot, replacedDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) <= retainedReplaced {
		return
	}
	slices.Sort(names)
	for _, name := range names[:len(names)-retainedReplaced] {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			s.mu.Lock()
			s.publishCleanupErrors++
			s.mu.Unlock()
		}
	}
}

// loadPublishedPaths returns the paths earlier publications added to the display
// surface. Paths already present in the floor are tracked implicitly and are not
// recorded here.
func (s *Store) loadPublishedPaths() (map[string]struct{}, error) {
	if s.liveRoot == "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := make(map[string]struct{}, len(s.publishedPaths))
		for path := range s.publishedPaths {
			out[path] = struct{}{}
		}
		return out, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.liveRoot, publishedPathsName))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("vfs: read published paths: %w", err)
	}
	var listed []string
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("vfs: decode published paths: %w", err)
	}
	out := make(map[string]struct{}, len(listed))
	for _, path := range listed {
		out[path] = struct{}{}
	}
	return out, nil
}

func (s *Store) savePublishedPaths(published map[string]struct{}) error {
	if s.liveRoot == "" {
		s.mu.Lock()
		s.publishedPaths = published
		s.mu.Unlock()
		return nil
	}
	listed := make([]string, 0, len(published))
	for path := range published {
		listed = append(listed, path)
	}
	slices.Sort(listed)
	payload, err := json.Marshal(listed)
	if err != nil {
		return fmt.Errorf("vfs: encode published paths: %w", err)
	}
	target := filepath.Join(s.liveRoot, publishedPathsName)
	temp := target + ".tmp"
	if err := os.WriteFile(temp, payload, 0o600); err != nil {
		return fmt.Errorf("vfs: write published paths: %w", err)
	}
	if err := os.Rename(temp, target); err != nil {
		return fmt.Errorf("vfs: install published paths: %w", err)
	}
	return nil
}

func (s *Store) invalidateFloorCache() {
	s.mu.Lock()
	s.baseFilesOnce = sync.Once{}
	s.baseFiles = nil
	s.baseDirs = nil
	s.baseFilesErr = nil
	s.mu.Unlock()
	s.epochMu.Lock()
	s.epoch = ""
	s.epochMu.Unlock()
}

// copyPublishedPath copies a single displaced entry into the replaced directory.
func copyPublishedPath(src, dst string) (retErr error) {
	copied := false
	defer func() {
		if copied {
			return
		}
		retErr = errors.Join(retErr, os.RemoveAll(dst))
	}()
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		retErr = os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()); err != nil {
			return err
		}
		_, retErr = copyTree(src, dst)
	case info.Mode().IsRegular():
		input, err := os.Open(src)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return errors.Join(err, input.Close())
		}
		_, copyErr := io.Copy(output, input)
		retErr = errors.Join(copyErr, output.Close(), input.Close())
	default:
		return fmt.Errorf("%w: %s", ErrSpecialFile, src)
	}
	copied = retErr == nil
	return retErr
}

// recordDisplayState refreshes the floor's record of what the display surface
// looks like, so a publication this store made never reads as an outside edit.
func (s *Store) recordDisplayState() error {
	if s.liveRoot == "" || s.displayDir == s.floorDir {
		return nil
	}
	digest, err := displayDigest(s.displayDir)
	if err != nil {
		return err
	}
	return writeFloorMeta(filepath.Join(s.liveRoot, floorMetaName), digest)
}
