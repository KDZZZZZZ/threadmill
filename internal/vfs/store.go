// Package vfs 为每个环境提供逻辑 overlay 文件视图。
// 写只落在本环境 overlay；读沿 overlay → parent → 只读 base。
// FileInfo 与 DirEnt 是本包本地类型（字段与 env 对齐），等 Env.Files 接线后再适配。
package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrInvalidPath 表示路径越出工作区或不是相对路径。
var (
	ErrInvalidPath       = errors.New("vfs: invalid path")
	ErrSpecialFile       = errors.New("vfs: special file not supported")
	ErrFileTooLarge      = errors.New("vfs: file too large")
	ErrTotalSizeExceeded = errors.New("vfs: total size exceeded")
)

// Default limits for VFS absorb/file operations.
const (
	MaxFileSize  = 50 * 1024 * 1024  // 50 MB
	MaxTotalSize = 200 * 1024 * 1024 // 200 MB
	// MergeRuntimeDir is visible only while a joined target reviews incoming files.
	MergeRuntimeDir = ".threadmill/runtime/joins"
)

// MergeSource identifies one child workspace offered to a joined target.
type MergeSource struct {
	Name  string
	EnvID string
}

// MergeChange describes one direct child change in a prepared merge.
type MergeChange struct {
	Source    string `json:"source"`
	Directory string `json:"directory"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Conflict  string `json:"conflict,omitempty"`
	Ours      bool   `json:"ours"`
	Theirs    bool   `json:"theirs"`
}

// MergeManifest tells the joined target which files were merged and which need review.
type MergeManifest struct {
	Changes []MergeChange `json:"changes"`
}

// FileInfo 是路径上的文件元数据。
type FileInfo struct {
	Name  string
	Size  int64
	IsDir bool
}

// DirEnt 是目录里的一项。
type DirEnt struct {
	Name  string
	IsDir bool
}

type blob struct {
	data       []byte
	tombstone  bool
	executable bool
}

type layer struct {
	parentID string
	files    map[string]blob
	baseline []map[string]blob // Fork 瞬间从父到根的 overlay 快照；nil 表示不是 Fork 出来的
}

// Store 按环境保存 overlay。Fork 拍父 overlay 快照作基线，不复制 host 树。
type Store struct {
	mu       sync.Mutex // ponytail: one store mutex, per-env locks if throughput matters
	baseDir  string
	liveRoot string
	envs     map[string]*layer
	lives    map[string]string
	merges   map[string]MergeManifest

	materializeCopies       uint64
	materializeCopyErrors   uint64
	materializeCopyDuration time.Duration
	handoffs                uint64
}

// Stats 是 VFS 当前持有的有界资源清单。
type Stats struct {
	Environments int   `json:"environments"`
	LiveDirs     int   `json:"live_dirs"`
	OverlayFiles int   `json:"overlay_files"`
	Tombstones   int   `json:"tombstones"`
	OverlayBytes int64 `json:"overlay_bytes"`

	MaterializeCopies       uint64        `json:"materialize_copies"`
	MaterializeCopyErrors   uint64        `json:"materialize_copy_errors"`
	MaterializeCopyDuration time.Duration `json:"materialize_copy_duration"`
	Handoffs                uint64        `json:"handoffs"`
}

// NewStore 以只读 host 树为 base。写入不会改 baseDir。
func NewStore(baseDir string) *Store {
	return &Store{
		baseDir: baseDir,
		envs:    make(map[string]*layer),
		lives:   make(map[string]string),
		merges:  make(map[string]MergeManifest),
	}
}

// NewPersistentStore keeps materialized environments under liveRoot so another
// Store instance can resume them after an ungraceful process exit.
func NewPersistentStore(baseDir, liveRoot string) (*Store, error) {
	if liveRoot == "" {
		return nil, fmt.Errorf("vfs: persistent live root is required")
	}
	if err := os.MkdirAll(liveRoot, 0o700); err != nil {
		return nil, fmt.Errorf("vfs: create persistent live root: %w", err)
	}
	root, err := confinedRoot(liveRoot)
	if err != nil {
		return nil, fmt.Errorf("vfs: open persistent live root: %w", err)
	}
	store := NewStore(baseDir)
	store.liveRoot = root
	return store, nil
}

// Stats 返回 overlay 和 live 目录的并发一致快照，不扫描宿主工作区。
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := Stats{
		Environments:            len(s.envs),
		LiveDirs:                len(s.lives),
		MaterializeCopies:       s.materializeCopies,
		MaterializeCopyErrors:   s.materializeCopyErrors,
		MaterializeCopyDuration: s.materializeCopyDuration,
		Handoffs:                s.handoffs,
	}
	for _, layer := range s.envs {
		for _, item := range layer.files {
			if item.tombstone {
				stats.Tombstones++
				continue
			}
			stats.OverlayFiles++
			stats.OverlayBytes += int64(len(item.data))
		}
	}
	for id := range s.lives {
		if _, exists := s.envs[id]; !exists {
			stats.Environments++
		}
	}
	return stats
}

// Fork 先把 parent 的 live 收进 overlay，再给 child 挂上当时从父到根的 overlay 快照作基线。
// 子环境已存在时不覆盖，也不改基线。parent 未物化则 Absorb 是空操作。
func (s *Store) Fork(parentID, childID string) error {
	if childID == "" {
		return nil
	}
	s.mu.Lock()
	if _, exists := s.envs[childID]; exists {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.Absorb(parentID); err != nil {
		return err
	}
	if live, ok, err := s.persistedLive(childID); err != nil {
		return err
	} else if ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, exists := s.envs[childID]; exists {
			return nil
		}
		s.envs[childID] = &layer{
			parentID: parentID,
			files:    make(map[string]blob),
			baseline: s.snapshotOverlays(parentID),
		}
		s.lives[childID] = live
		return nil
	}
	s.mu.Lock()
	if _, exists := s.envs[childID]; exists {
		s.mu.Unlock()
		return nil
	}
	s.envs[childID] = &layer{
		parentID: parentID,
		files:    make(map[string]blob),
		baseline: s.snapshotOverlays(parentID),
	}
	persistent := s.liveRoot != ""
	s.mu.Unlock()
	if persistent {
		_, err := s.Materialize(childID)
		return err
	}
	return nil
}

// Handoff forks parent into child by moving an existing materialized workspace.
// It is for a single successor after parent has stopped running; when no live
// workspace exists it falls back to an ordinary logical fork.
func (s *Store) Handoff(parentID, childID string) error {
	if childID == "" || childID == parentID {
		return nil
	}

	s.mu.Lock()
	if _, exists := s.envs[childID]; exists {
		s.mu.Unlock()
		return nil
	}
	if live, ok, err := s.persistedLive(childID); err != nil {
		s.mu.Unlock()
		return err
	} else if ok {
		s.envs[childID] = &layer{
			parentID: parentID,
			files:    make(map[string]blob),
			baseline: s.snapshotOverlays(parentID),
		}
		s.lives[childID] = live
		s.mu.Unlock()
		return nil
	}

	live := s.lives[parentID]
	if live == "" {
		var ok bool
		var err error
		live, ok, err = s.persistedLive(parentID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if !ok {
			s.mu.Unlock()
			return s.Fork(parentID, childID)
		}
	}

	childLive := live
	if s.liveRoot != "" {
		childLive = s.persistentLivePath(childID)
		if err := os.Rename(live, childLive); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("vfs: handoff persistent environment: %w", err)
		}
	}
	s.envs[childID] = &layer{
		parentID: parentID,
		files:    make(map[string]blob),
		baseline: s.snapshotOverlays(parentID),
	}
	delete(s.lives, parentID)
	s.lives[childID] = childLive
	s.handoffs++
	s.mu.Unlock()
	return nil
}

func (s *Store) persistedLive(envID string) (string, bool, error) {
	if s.liveRoot == "" || envID == "" {
		return "", false, nil
	}
	path := s.persistentLivePath(envID)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return "", false, fmt.Errorf("vfs: inspect persistent environment: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("vfs: persistent environment is not a directory: %s", path)
	}
	return path, true, nil
}

func (s *Store) persistentLivePath(envID string) string {
	digest := sha256.Sum256([]byte(envID))
	return filepath.Join(s.liveRoot, fmt.Sprintf("%x", digest[:]))
}

func (s *Store) persistentMergePath(envID string) string {
	digest := sha256.Sum256([]byte(envID))
	return filepath.Join(s.liveRoot, fmt.Sprintf("%x.merge.json", digest[:]))
}

func (s *Store) persistedMerge(envID string) (MergeManifest, bool, error) {
	if s.liveRoot == "" || envID == "" {
		return MergeManifest{}, false, nil
	}
	data, err := os.ReadFile(s.persistentMergePath(envID))
	if os.IsNotExist(err) {
		return MergeManifest{}, false, nil
	}
	if err != nil {
		return MergeManifest{}, false, fmt.Errorf("vfs: read persistent merge: %w", err)
	}
	var manifest MergeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return MergeManifest{}, false, fmt.Errorf("vfs: decode persistent merge: %w", err)
	}
	return manifest, true, nil
}

func (s *Store) persistMerge(envID string, data []byte) error {
	if s.liveRoot == "" || envID == "" {
		return nil
	}
	tmp, err := os.CreateTemp(s.liveRoot, ".merge-")
	if err != nil {
		return fmt.Errorf("vfs: create persistent merge: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vfs: protect persistent merge: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vfs: write persistent merge: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("vfs: sync persistent merge: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("vfs: close persistent merge: %w", err)
	}
	if err := os.Rename(tmpName, s.persistentMergePath(envID)); err != nil {
		return fmt.Errorf("vfs: commit persistent merge: %w", err)
	}
	return nil
}

// Merge 把 from 的 overlay 增量三路并入 into。冲突失败，不改 into。
// 合入前先把双方 live 收进 overlay。
func (s *Store) Merge(from, into string) error {
	if into == "" {
		return nil
	}
	if err := s.Absorb(from); err != nil {
		return err
	}
	if err := s.Absorb(into); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	apply, conflicts := s.mergePlanLocked(from, into)
	if len(conflicts) > 0 {
		return fmt.Errorf("vfs: merge conflict: %s", conflicts[0])
	}
	return s.applyPendingLocked(into, apply)
}

type pending struct {
	path string
	b    blob
}

func (s *Store) mergePlanLocked(from, into string) ([]pending, []string) {
	fromLayer := s.envs[from]
	if fromLayer == nil {
		return nil, nil
	}
	childFiles := fromLayer.files
	paths := make([]string, 0, len(childFiles))
	for path := range childFiles {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	conflictSet := make(map[string]struct{})
	addConflict := func(path string) {
		conflictSet[path] = struct{}{}
	}
	for _, path := range paths {
		if childFiles[path].tombstone {
			continue
		}
		prefix := path + "/"
		for _, other := range paths {
			if other == path || childFiles[other].tombstone || !strings.HasPrefix(other, prefix) {
				continue
			}
			addConflict(other)
		}
	}

	var apply []pending
	for _, path := range paths {
		theirsBlob := childFiles[path]
		theirs := overlayContent(theirsBlob)
		base := s.mergeBase(fromLayer, fromLayer.parentID, path)
		ours := s.lookupContent(into, path)
		sameAncestor := contentEqual(theirs, base) || contentEqual(ours, theirs)
		pathConflict := false
		if !sameAncestor && !contentEqual(ours, base) {
			addConflict(path)
			pathConflict = true
		}
		needApply := !sameAncestor
		for _, q := range s.knownDescendants(into, fromLayer.baseline, path) {
			tq := s.lookupContent(from, q)
			bq := s.mergeBase(fromLayer, fromLayer.parentID, q)
			oq := s.lookupContent(into, q)
			if contentEqual(tq, bq) || contentEqual(oq, tq) {
				continue
			}
			if !contentEqual(oq, bq) {
				addConflict(q)
				pathConflict = true
				continue
			}
			needApply = true
		}
		if pathConflict || !needApply {
			continue
		}
		if !theirs.tombstone && s.liveFileAncestor(into, path) {
			addConflict(path)
			continue
		}
		apply = append(apply, pending{path: path, b: cloneBlob(theirsBlob)})
	}

	conflicts := make([]string, 0, len(conflictSet))
	for path := range conflictSet {
		conflicts = append(conflicts, path)
	}
	slices.Sort(conflicts)
	apply = slices.DeleteFunc(apply, func(e pending) bool {
		for _, conflict := range conflicts {
			if pathsOverlap(e.path, conflict) {
				return true
			}
		}
		return false
	})
	return apply, conflicts
}

func (s *Store) applyPendingLocked(into string, apply []pending) error {
	if len(apply) == 0 {
		return nil
	}
	dst := s.ensure(into)
	for _, e := range apply {
		if e.b.tombstone {
			applyBlob(dst, e.path, e.b)
		}
	}
	for _, e := range apply {
		if !e.b.tombstone {
			applyBlob(dst, e.path, e.b)
		}
	}
	if live, ok := s.lives[into]; ok {
		for _, e := range apply {
			if !e.b.tombstone {
				continue
			}
			if err := applyLive(live, e.path, e.b); err != nil {
				return err
			}
		}
		for _, e := range apply {
			if e.b.tombstone {
				continue
			}
			if err := applyLive(live, e.path, e.b); err != nil {
				return err
			}
		}
	}
	return nil
}

// PrepareMerge applies independent changes to workspaceID and exposes both sides of
// conflicting files in a reserved runtime directory for the target role to inspect.
func (s *Store) PrepareMerge(workspaceID string, sources []MergeSource) (MergeManifest, error) {
	s.mu.Lock()
	manifest, prepared := s.merges[workspaceID]
	manifest = cloneMergeManifest(manifest)
	s.mu.Unlock()
	if !prepared {
		persisted, ok, err := s.persistedMerge(workspaceID)
		if err != nil {
			return MergeManifest{}, err
		}
		if ok {
			manifest = cloneMergeManifest(persisted)
			prepared = true
			s.mu.Lock()
			s.merges[workspaceID] = cloneMergeManifest(manifest)
			s.mu.Unlock()
		}
	}

	live, err := s.Materialize(workspaceID)
	if err != nil {
		return MergeManifest{}, err
	}
	runtime := filepath.Join(live, filepath.FromSlash(MergeRuntimeDir))
	if err := os.RemoveAll(runtime); err != nil {
		return MergeManifest{}, fmt.Errorf("vfs: prepare merge: reset runtime: %w", err)
	}

	if !prepared {
		manifest = MergeManifest{Changes: []MergeChange{}}
	}
	for i, source := range sources {
		if err := s.Absorb(source.EnvID); err != nil {
			return MergeManifest{}, err
		}
		directory := fmt.Sprintf("source-%d", i+1)

		s.mu.Lock()
		fromLayer := s.envs[source.EnvID]
		var paths []string
		var sourceFiles map[string]blob
		if fromLayer != nil {
			sourceFiles = cloneFiles(fromLayer.files)
			paths = make([]string, 0, len(sourceFiles))
			for path := range sourceFiles {
				paths = append(paths, path)
			}
		}
		slices.Sort(paths)
		for _, path := range paths {
			if isMergeRuntimePath(path) {
				s.mu.Unlock()
				return MergeManifest{}, fmt.Errorf("vfs: prepare merge: reserved path %q", path)
			}
		}
		var apply []pending
		var conflicts []string
		if !prepared {
			apply, conflicts = s.mergePlanLocked(source.EnvID, workspaceID)
		}
		s.mu.Unlock()

		for _, path := range paths {
			ours, err := copyMergeSide(live, filepath.Join(runtime, "ours", directory), path)
			if err != nil {
				return MergeManifest{}, err
			}
			theirs, err := copyMergeBlob(filepath.Join(runtime, "sources", directory), path, sourceFiles[path])
			if err != nil {
				return MergeManifest{}, err
			}
			if prepared {
				continue
			}
			change := MergeChange{
				Source:    source.Name,
				Directory: directory,
				Path:      path,
				Kind:      mergeChangeKind(ours, theirs),
				Status:    "merged",
				Ours:      ours,
				Theirs:    theirs,
			}
			for _, conflict := range conflicts {
				if pathsOverlap(path, conflict) {
					change.Status = "conflict"
					change.Conflict = conflict
					break
				}
			}
			manifest.Changes = append(manifest.Changes, change)
		}

		if !prepared {
			s.mu.Lock()
			if err := s.applyPendingLocked(workspaceID, apply); err != nil {
				s.mu.Unlock()
				return MergeManifest{}, err
			}
			s.mu.Unlock()
		}
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return MergeManifest{}, fmt.Errorf("vfs: prepare merge: encode manifest: %w", err)
	}
	if !prepared {
		if err := s.persistMerge(workspaceID, data); err != nil {
			return MergeManifest{}, err
		}
	}
	if err := writeLive(live, MergeRuntimeDir+"/manifest.json", data); err != nil {
		return MergeManifest{}, fmt.Errorf("vfs: prepare merge: write manifest: %w", err)
	}
	if !prepared {
		s.mu.Lock()
		s.merges[workspaceID] = cloneMergeManifest(manifest)
		s.mu.Unlock()
	}
	return manifest, nil
}

// CommitMerge removes temporary evidence and merges the target's reviewed result.
func (s *Store) CommitMerge(workspaceID, targetID string) error {
	s.mu.Lock()
	live := s.lives[workspaceID]
	s.mu.Unlock()
	if live != "" {
		if err := os.RemoveAll(filepath.Join(live, filepath.FromSlash(MergeRuntimeDir))); err != nil {
			return fmt.Errorf("vfs: commit merge: remove runtime: %w", err)
		}
	}
	if err := s.Release(workspaceID); err != nil {
		return err
	}
	return s.Merge(workspaceID, targetID)
}

func copyMergeSide(root, dstRoot, rel string) (bool, error) {
	src := filepath.Join(root, filepath.FromSlash(rel))
	if escapesRoot(root, src) {
		return false, fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	info, err := os.Lstat(src)
	if errors.Is(err, syscall.ENOTDIR) {
		for _, ancestor := range ancestorPrefixes(rel) {
			copied, copyErr := copyMergeSide(root, dstRoot, ancestor)
			if copyErr != nil {
				return false, copyErr
			}
			if copied {
				break
			}
		}
		return false, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
	if escapesRoot(dstRoot, dst) {
		return false, fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o750); err != nil {
			return false, err
		}
		if err := copyTree(src, dst); err != nil {
			return false, err
		}
		return true, nil
	}
	if info.Mode().Type() != 0 {
		return false, fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, info.Mode().String())
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, data, regularMode(info.Mode().Perm()&0o111 != 0)); err != nil {
		return false, err
	}
	return true, nil
}

func copyMergeBlob(dstRoot, rel string, b blob) (bool, error) {
	if b.tombstone {
		return false, nil
	}
	dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
	if escapesRoot(dstRoot, dst) {
		return false, fmt.Errorf("%w: %q", ErrInvalidPath, rel)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, b.data, regularMode(b.executable)); err != nil {
		return false, err
	}
	return true, nil
}

func cloneMergeManifest(manifest MergeManifest) MergeManifest {
	return MergeManifest{Changes: slices.Clone(manifest.Changes)}
}

func mergeChangeKind(ours, theirs bool) string {
	switch {
	case !ours && theirs:
		return "add"
	case ours && !theirs:
		return "delete"
	default:
		return "modify"
	}
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func isMergeRuntimePath(path string) bool {
	return path == MergeRuntimeDir || strings.HasPrefix(path, MergeRuntimeDir+"/")
}

type content struct {
	exists     bool
	tombstone  bool
	executable bool
	data       []byte
	maskFrom   string
	maskFile   bool
}

func overlayContent(b blob) content {
	return content{
		exists:     true,
		tombstone:  b.tombstone,
		executable: b.executable,
		data:       b.data,
	}
}

func maskedContent(prefix string, b blob) content {
	c := content{exists: true, tombstone: true, maskFrom: prefix, maskFile: !b.tombstone}
	if !b.tombstone {
		c.data = cloneBytes(b.data)
		c.executable = b.executable
	}
	return c
}

func (s *Store) mergeBase(fromLayer *layer, parentID, rel string) content {
	if fromLayer != nil && fromLayer.baseline != nil {
		return s.lookupFrozen(fromLayer.baseline, rel)
	}
	return s.lookupContent(parentID, rel)
}

func (s *Store) snapshotOverlays(envID string) []map[string]blob {
	if envID == "" {
		return nil
	}
	l, ok := s.envs[envID]
	if !ok {
		return []map[string]blob{}
	}
	out := []map[string]blob{cloneFiles(l.files)}
	if l.baseline != nil {
		for _, m := range l.baseline {
			out = append(out, cloneFiles(m))
		}
	} else if l.parentID != "" {
		out = append(out, s.snapshotOverlays(l.parentID)...)
	}
	return out
}

func (s *Store) overlayMaps(envID string) []map[string]blob {
	l, ok := s.envs[envID]
	if !ok {
		return nil
	}
	out := make([]map[string]blob, 0, 1+len(l.baseline))
	out = append(out, l.files)
	if l.baseline != nil {
		out = append(out, l.baseline...)
	} else if l.parentID != "" {
		out = append(out, s.overlayMaps(l.parentID)...)
	}
	return out
}

func (s *Store) lookupFrozen(chain []map[string]blob, rel string) content {
	for _, files := range chain {
		if b, ok := files[rel]; ok {
			return overlayContent(b)
		}
		if c, ok := filesMask(files, rel); ok {
			return c
		}
	}
	return s.lookupHost(rel)
}

func (s *Store) lookupContent(envID, rel string) content {
	return s.lookupFrozen(s.overlayMaps(envID), rel)
}

func (s *Store) lookupHost(rel string) content {
	host, err := s.resolveHost(rel)
	if err != nil {
		return content{}
	}
	info, err := os.Stat(host)
	if err != nil || info.IsDir() || info.Mode().Type() != 0 {
		return content{}
	}
	data, err := os.ReadFile(host)
	if err != nil {
		return content{}
	}
	return content{
		exists:     true,
		executable: info.Mode().Perm()&0o111 != 0,
		data:       data,
	}
}

func (s *Store) liveFileAncestor(envID, rel string) bool {
	for _, prefix := range ancestorPrefixes(rel) {
		c := s.lookupContent(envID, prefix)
		if c.exists && !c.tombstone && c.maskFrom == "" {
			return true
		}
	}
	return false
}

func (s *Store) knownDescendants(into string, baseline []map[string]blob, path string) []string {
	prefix := path + "/"
	seen := map[string]struct{}{}
	add := func(files map[string]blob) {
		for k := range files {
			if strings.HasPrefix(k, prefix) {
				seen[k] = struct{}{}
			}
		}
	}
	for _, files := range s.overlayMaps(into) {
		add(files)
	}
	for _, files := range baseline {
		add(files)
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func applyBlob(dst *layer, path string, b blob) {
	if b.tombstone {
		dst.files[path] = blob{tombstone: true}
	} else {
		dst.files[path] = cloneBlob(b)
	}
	prefix := path + "/"
	for k := range dst.files {
		if strings.HasPrefix(k, prefix) {
			delete(dst.files, k)
		}
	}
}

func contentEqual(a, b content) bool {
	if a.maskFrom != "" || b.maskFrom != "" {
		if a.maskFrom != b.maskFrom || a.maskFile != b.maskFile {
			return false
		}
		if a.maskFile {
			return a.executable == b.executable && bytes.Equal(a.data, b.data)
		}
		return true
	}
	if exactHidden(a) && exactHidden(b) {
		return true
	}
	if !a.exists && !b.exists {
		return true
	}
	if a.exists != b.exists || a.tombstone != b.tombstone {
		return false
	}
	if a.tombstone {
		return true
	}
	return a.executable == b.executable && bytes.Equal(a.data, b.data)
}

func exactHidden(c content) bool {
	return !c.exists || c.tombstone
}

func cloneBlob(b blob) blob {
	if b.tombstone {
		return blob{tombstone: true}
	}
	return blob{data: cloneBytes(b.data), executable: b.executable}
}

func cloneFiles(src map[string]blob) map[string]blob {
	dst := make(map[string]blob, len(src))
	for path, b := range src {
		dst[path] = cloneBlob(b)
	}
	return dst
}

// View 返回该环境的文件视图。
func (s *Store) View(envID string) *View {
	return &View{store: s, envID: envID}
}

// View 是某个环境在 Store 上的文件视图。
type View struct {
	store *Store
	envID string
}

func (v *View) liveDir() (string, bool) {
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	dir, ok := v.store.lives[v.envID]
	return dir, ok
}

// Read 沿 overlay → parent → base 读文件，返回拷贝。已物化则读 live。
func (v *View) Read(path string) ([]byte, error) {
	rel, err := jail(path)
	if err != nil {
		return nil, err
	}
	if live, ok := v.liveDir(); ok {
		return readLive(live, rel)
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()

	if data, tombstone, found := v.store.lookupBlob(v.envID, rel); found {
		if tombstone {
			if v.store.hasOverlayChildren(v.envID, rel) {
				return nil, fmt.Errorf("vfs: %s: is a directory", rel)
			}
			return nil, notFound(rel)
		}
		return cloneBytes(data), nil
	}
	if v.store.hasOverlayChildren(v.envID, rel) {
		return nil, fmt.Errorf("vfs: %s: is a directory", rel)
	}

	host, err := v.store.resolveHost(rel)
	if err != nil {
		return nil, mapHostError("read", rel, err)
	}
	data, err := os.ReadFile(host)
	if err != nil {
		return nil, mapIOError("read", rel, err)
	}
	return cloneBytes(data), nil
}

// Write 写入本环境 overlay；已物化则写 live。
func (v *View) Write(path string, data []byte) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	if live, ok := v.liveDir(); ok {
		return writeLive(live, rel, data)
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	current := v.store.lookupContent(v.envID, rel)
	v.store.ensure(v.envID).files[rel] = blob{
		data: cloneBytes(data),
		executable: current.exists &&
			!current.tombstone &&
			current.maskFrom == "" &&
			current.executable,
	}
	return nil
}

// Delete 在本环境 overlay 上打 tombstone；已物化则删 live。
func (v *View) Delete(path string) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	if live, ok := v.liveDir(); ok {
		return deleteLive(live, rel)
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	v.store.ensure(v.envID).files[rel] = blob{tombstone: true}
	return nil
}

// Stat 沿 overlay → parent → base 返回元数据。已物化则看 live。
func (v *View) Stat(path string) (FileInfo, error) {
	rel, err := jail(path)
	if err != nil {
		return FileInfo{}, err
	}
	if live, ok := v.liveDir(); ok {
		return statLive(live, rel)
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()

	if info, handled, err := v.store.lookupStat(v.envID, rel); handled {
		return info, err
	}

	host, err := v.store.resolveHost(rel)
	if err != nil {
		return FileInfo{}, mapHostError("stat", rel, err)
	}
	fi, err := os.Stat(host)
	if err != nil {
		return FileInfo{}, mapIOError("stat", rel, err)
	}
	return FileInfo{Name: fi.Name(), Size: fi.Size(), IsDir: fi.IsDir()}, nil
}

// List 合并 base 目录项与 overlay（tombstone 会去掉对应名字）。已物化则列 live。
func (v *View) List(path string) ([]DirEnt, error) {
	rel, err := jail(path)
	if err != nil {
		return nil, err
	}
	if live, ok := v.liveDir(); ok {
		return listLive(live, rel)
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()

	if _, tombstone, found := v.store.lookupBlob(v.envID, rel); found {
		if tombstone {
			if !v.store.hasOverlayChildren(v.envID, rel) {
				return nil, notFound(rel)
			}
			ents := map[string]DirEnt{}
			v.store.applyOverlayList(v.envID, rel, ents)
			return sortedDirents(ents), nil
		}
		return nil, fmt.Errorf("vfs: %s: not a directory", rel)
	}

	ents := map[string]DirEnt{}
	exists := false

	host, err := v.store.resolveHost(rel)
	switch {
	case err == nil:
		fi, statErr := os.Stat(host)
		if statErr != nil {
			return nil, mapIOError("list", rel, statErr)
		}
		if fi.IsDir() {
			exists = true
			entries, readErr := os.ReadDir(host)
			if readErr != nil {
				return nil, mapIOError("list", rel, readErr)
			}
			for _, e := range entries {
				ents[e.Name()] = DirEnt{Name: e.Name(), IsDir: e.IsDir()}
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// overlay 里可能仍有这个目录
	case errors.Is(err, ErrInvalidPath):
		return nil, err
	default:
		return nil, mapIOError("list", rel, err)
	}

	v.store.applyOverlayList(v.envID, rel, ents)
	if len(ents) > 0 {
		exists = true
	}
	if !exists && !v.store.hasOverlayChildren(v.envID, rel) {
		return nil, notFound(rel)
	}

	return sortedDirents(ents), nil
}

func (s *Store) ensure(envID string) *layer {
	if l, ok := s.envs[envID]; ok {
		return l
	}
	l := &layer{files: make(map[string]blob)}
	s.envs[envID] = l
	return l
}

func (s *Store) lookupBlob(envID, rel string) ([]byte, bool, bool) {
	b, found := s.lookupBlobValue(envID, rel)
	return b.data, b.tombstone, found
}

func (s *Store) lookupBlobValue(envID, rel string) (blob, bool) {
	for _, files := range s.overlayMaps(envID) {
		if b, ok := files[rel]; ok {
			return b, true
		}
		if _, ok := filesMask(files, rel); ok {
			return blob{tombstone: true}, true
		}
	}
	return blob{}, false
}

func layerMasks(l *layer, rel string) bool {
	_, ok := filesMask(l.files, rel)
	return ok
}

func filesMask(files map[string]blob, rel string) (content, bool) {
	for _, prefix := range ancestorPrefixes(rel) {
		if b, ok := files[prefix]; ok {
			return maskedContent(prefix, b), true
		}
	}
	return content{}, false
}

func ancestorPrefixes(rel string) []string {
	var out []string
	for {
		i := strings.LastIndex(rel, "/")
		if i <= 0 {
			return out
		}
		rel = rel[:i]
		out = append(out, rel)
	}
}

func (s *Store) lookupStat(envID, rel string) (FileInfo, bool, error) {
	if data, tombstone, found := s.lookupBlob(envID, rel); found {
		if tombstone {
			if rel != "." && s.hasOverlayChildren(envID, rel) {
				return FileInfo{Name: filepath.Base(rel), IsDir: true}, true, nil
			}
			return FileInfo{}, true, notFound(rel)
		}
		return FileInfo{
			Name:  filepath.Base(rel),
			Size:  int64(len(data)),
			IsDir: false,
		}, true, nil
	}
	if rel != "." && s.hasOverlayChildren(envID, rel) {
		return FileInfo{Name: filepath.Base(rel), IsDir: true}, true, nil
	}
	return FileInfo{}, false, nil
}

func (s *Store) hasOverlayChildren(envID, rel string) bool {
	ents := map[string]DirEnt{}
	s.applyOverlayList(envID, rel, ents)
	return len(ents) > 0
}

func (s *Store) applyOverlayList(envID, rel string, ents map[string]DirEnt) {
	maps := s.overlayMaps(envID)
	for i := len(maps) - 1; i >= 0; i-- {
		files := maps[i]
		if _, ok := files[rel]; ok {
			for name := range ents {
				delete(ents, name)
			}
		}
		applyLayerList(files, rel, ents, true)
		applyLayerList(files, rel, ents, false)
	}
}

func applyLayerList(files map[string]blob, rel string, ents map[string]DirEnt, tombstones bool) {
	for filePath, b := range files {
		if b.tombstone != tombstones {
			continue
		}
		name, isDir, ok := listPart(rel, filePath)
		if !ok {
			continue
		}
		if b.tombstone {
			if !isDir {
				delete(ents, name)
			}
			continue
		}
		ents[name] = DirEnt{Name: name, IsDir: isDir}
	}
}

func (s *Store) resolveHost(rel string) (string, error) {
	root, err := confinedRoot(s.baseDir)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(root, filepath.FromSlash(rel))
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

func confinedRoot(baseDir string) (string, error) {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func escapesRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func jail(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || !filepath.IsLocal(path) {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, path)
	}
	return filepath.ToSlash(filepath.Clean(path)), nil
}

func sortedDirents(ents map[string]DirEnt) []DirEnt {
	names := make([]string, 0, len(ents))
	for name := range ents {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]DirEnt, 0, len(names))
	for _, name := range names {
		out = append(out, ents[name])
	}
	return out
}

func listPart(dir, filePath string) (name string, isDir bool, ok bool) {
	if dir == "." {
		dir = ""
	}
	rest := filePath
	if dir != "" {
		if filePath == dir {
			return "", false, false
		}
		prefix := dir + "/"
		if !strings.HasPrefix(filePath, prefix) {
			return "", false, false
		}
		rest = filePath[len(prefix):]
	}
	name, _, found := strings.Cut(rest, "/")
	if name == "" || name == "." {
		return "", false, false
	}
	return name, found, true
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func notFound(rel string) error {
	return fmt.Errorf("vfs: %s: %w", rel, fs.ErrNotExist)
}

func mapIOError(op, rel string, err error) error {
	if os.IsNotExist(err) {
		return notFound(rel)
	}
	return fmt.Errorf("vfs: %s %s: %w", op, rel, err)
}

func mapHostError(op, rel string, err error) error {
	if errors.Is(err, ErrInvalidPath) || errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return mapIOError(op, rel, err)
}
