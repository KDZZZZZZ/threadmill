// Package vfs 为每个环境提供逻辑 overlay 文件视图。
// 写只落在本环境 overlay；读沿 overlay → parent → 只读 base。
// FileInfo 与 DirEnt 是本包本地类型（字段与 env 对齐），等 Env.Files 接线后再适配。
package vfs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrInvalidPath 表示路径越出工作区或不是相对路径。
var (
	ErrInvalidPath        = errors.New("vfs: invalid path")
	ErrUnknownEnvironment = errors.New("vfs: unknown environment")
	ErrSpecialFile        = errors.New("vfs: special file not supported")
	ErrFileTooLarge       = errors.New("vfs: file too large")
	ErrTotalSizeExceeded  = errors.New("vfs: total size exceeded")
)

// Default limits for VFS absorb/file operations.
const (
	MaxFileSize               = 50 * 1024 * 1024  // 50 MB
	MaxTotalSize              = 200 * 1024 * 1024 // 200 MB
	maxMaterializeCopies      = 8
	defaultOverlayLimit       = 128
	defaultNativeOverlayLimit = 1024
)

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

type materializeCall struct {
	done chan struct{}
	dir  string
	err  error
}

// Options selects optional VFS acceleration without changing visible file semantics.
type Options struct {
	Overlay      bool
	OverlayLimit int
}

// Store 按环境保存 overlay。Fork 拍父 overlay 快照作基线，不复制 host 树。
type Store struct {
	mu sync.Mutex // ponytail: one store mutex, per-env locks if throughput matters
	// floorDir is the immutable tree every environment reads through and the
	// overlay lower directory. displayDir is where Publish renders checkpoints
	// for the user. A persistent store keeps them apart so publication never
	// mutates what a running environment reads; NewStore collapses them, which
	// only suits ephemeral stores with no environment outliving a publication.
	floorDir      string
	displayDir    string
	liveRoot      string
	envs          map[string]*layer
	lives         map[string]string
	liveBaselines map[string]*liveFingerprint // 物化完成时的分桶 stat 向量；恢复目录首次仍做内容扫描
	materializing map[string]*materializeCall
	ioSlots       chan struct{}
	overlay       *overlayDriver
	overlaySlots  chan struct{}
	mountMu       sync.Mutex
	mounts        map[string]*overlayMount

	epochMu sync.Mutex
	epoch   string // 基线仓一次性 stat 纪元，见 fingerprint.go

	// publishedPaths tracks display paths added by earlier publications when the
	// store has no live root to persist them to; see publish.go.
	publishedPaths map[string]struct{}

	baseFilesOnce sync.Once
	baseFiles     map[string]fileSnapshot
	baseDirs      map[string]struct{}
	baseFilesErr  error

	materializeCopies        uint64
	materializeOverlays      uint64
	materializeReflinks      uint64
	materializeFullCopies    uint64
	materializeFallbacks     uint64
	overlayCapacityFallbacks uint64
	overlayErrorFallbacks    uint64
	overlayLastFallback      string
	materializeCopyErrors    uint64
	materializeCopyDuration  time.Duration
	materializeActive        int
	materializePeakActive    int
	materializeWaitDuration  time.Duration
	absorbFastPaths          uint64
	absorbUpperAttempts      uint64
	absorbUpperEntries       uint64
	absorbUpperFallbacks     uint64
	absorbUpperErrors        uint64
	absorbUpperDuration      time.Duration
	absorbScans              uint64
	absorbScanErrors         uint64
	absorbScanDuration       time.Duration
	absorbContentComparisons uint64
	absorbActive             int
	absorbPeakActive         int
	absorbWaitDuration       time.Duration
	handoffs                 uint64
	publishAttempts          uint64
	publishCommits           uint64
	publishErrors            uint64
	publishCleanupErrors     uint64
	publishDuration          time.Duration
}

// Stats 是 VFS 当前持有的有界资源清单。
type Stats struct {
	Environments int   `json:"environments"`
	LiveDirs     int   `json:"live_dirs"`
	OverlayFiles int   `json:"overlay_files"`
	Tombstones   int   `json:"tombstones"`
	OverlayBytes int64 `json:"overlay_bytes"`

	MaterializeCopies        uint64        `json:"materialize_copies"`
	MaterializeOverlays      uint64        `json:"materialize_overlays"`
	MaterializeReflinks      uint64        `json:"materialize_reflinks"`
	MaterializeFullCopies    uint64        `json:"materialize_full_copies"`
	MaterializeFallbacks     uint64        `json:"materialize_fallbacks"`
	OverlayCapacityFallbacks uint64        `json:"overlay_capacity_fallbacks"`
	OverlayErrorFallbacks    uint64        `json:"overlay_error_fallbacks"`
	OverlayLastFallback      string        `json:"overlay_last_fallback,omitempty"`
	MaterializeCopyErrors    uint64        `json:"materialize_copy_errors"`
	MaterializeCopyDuration  time.Duration `json:"materialize_copy_duration"`
	MaterializeCapacity      int           `json:"materialize_capacity"`
	MaterializeActive        int           `json:"materialize_active"`
	MaterializePeakActive    int           `json:"materialize_peak_active"`
	MaterializeWaitDuration  time.Duration `json:"materialize_wait_duration"`
	OverlayAvailable         bool          `json:"overlay_available"`
	OverlayBackend           string        `json:"overlay_backend"`
	OverlayActive            int           `json:"overlay_active"`
	OverlayCapacity          int           `json:"overlay_capacity"`
	AbsorbFastPaths          uint64        `json:"absorb_fast_paths"`
	AbsorbUpperAttempts      uint64        `json:"absorb_upper_attempts"`
	AbsorbUpperEntries       uint64        `json:"absorb_upper_entries"`
	AbsorbUpperFallbacks     uint64        `json:"absorb_upper_fallbacks"`
	AbsorbUpperErrors        uint64        `json:"absorb_upper_errors"`
	AbsorbUpperDuration      time.Duration `json:"absorb_upper_duration"`
	AbsorbScans              uint64        `json:"absorb_scans"`
	AbsorbScanErrors         uint64        `json:"absorb_scan_errors"`
	AbsorbScanDuration       time.Duration `json:"absorb_scan_duration"`
	AbsorbContentComparisons uint64        `json:"absorb_content_comparisons"`
	AbsorbCapacity           int           `json:"absorb_capacity"`
	AbsorbActive             int           `json:"absorb_active"`
	AbsorbPeakActive         int           `json:"absorb_peak_active"`
	AbsorbWaitDuration       time.Duration `json:"absorb_wait_duration"`
	Handoffs                 uint64        `json:"handoffs"`
	PublishAttempts          uint64        `json:"publish_attempts"`
	PublishCommits           uint64        `json:"publish_commits"`
	PublishErrors            uint64        `json:"publish_errors"`
	PublishCleanupErrors     uint64        `json:"publish_cleanup_errors"`
	PublishDuration          time.Duration `json:"publish_duration"`
}

// NewStore 以 host 树为 base。环境执行期间 base 不变；只有 Publish 会提交最终结果。
// NewStore reads through dir and also publishes into it. Environments must not
// outlive a publication in this mode; NewPersistentStore separates the two.
func NewStore(dir string) *Store {
	materializeCapacity := min(runtime.NumCPU(), maxMaterializeCopies)
	return &Store{
		floorDir:      dir,
		displayDir:    dir,
		envs:          make(map[string]*layer),
		lives:         make(map[string]string),
		liveBaselines: make(map[string]*liveFingerprint),
		materializing: make(map[string]*materializeCall),
		ioSlots:       make(chan struct{}, max(1, materializeCapacity)),
		mounts:        make(map[string]*overlayMount),
	}
}

// WorkspaceRoot returns the canonical host path backing every environment: the
// read floor, not the display surface. Execution backends use it only as a mount
// target; agents never receive the backing tree directly.
func (s *Store) WorkspaceRoot() (string, error) {
	if s == nil {
		return "", fmt.Errorf("vfs: nil store")
	}
	return confinedRoot(s.floorDir)
}

// NewPersistentStore keeps materialized environments under liveRoot so another
// Store instance can resume them after an ungraceful process exit.
func NewPersistentStore(projectDir, liveRoot string) (*Store, error) {
	return NewPersistentStoreWithOptions(projectDir, liveRoot, Options{})
}

// NewPersistentStoreWithOptions enables optional acceleration for a persistent store.
func NewPersistentStoreWithOptions(
	projectDir, liveRoot string,
	options Options,
) (*Store, error) {
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
	floor, err := prepareFloor(projectDir, root)
	if err != nil {
		return nil, err
	}
	store := NewStore(floor)
	store.displayDir = projectDir
	store.liveRoot = root
	if options.Overlay {
		store.overlay = detectOverlayDriver()
		// The floor lives under liveRoot, so a reflink clone is available
		// whenever the filesystem supports it at all.
		if store.overlay != nil && !ReflinkCloneable(floor, root) {
			limit := options.OverlayLimit
			if limit <= 0 {
				limit = defaultOverlayLimit
				if store.overlay.kind == "native-overlayfs" {
					limit = defaultNativeOverlayLimit
				}
			}
			store.overlaySlots = make(chan struct{}, limit)
		} else {
			store.overlay = nil
		}
	}
	return store, nil
}

// Close unmounts active acceleration backends while retaining persistent state.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mountMu.Lock()
	ids := make([]string, 0, len(s.mounts))
	for id := range s.mounts {
		ids = append(ids, id)
	}
	s.mountMu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := s.closeOverlay(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Stats 返回 overlay 和 live 目录的并发一致快照，不扫描宿主工作区。
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	overlayBackend := "unavailable"
	if s.overlay != nil {
		overlayBackend = s.overlay.kind
	}
	stats := Stats{
		Environments:             len(s.envs),
		LiveDirs:                 len(s.lives),
		MaterializeCopies:        s.materializeCopies,
		MaterializeOverlays:      s.materializeOverlays,
		MaterializeReflinks:      s.materializeReflinks,
		MaterializeFullCopies:    s.materializeFullCopies,
		MaterializeFallbacks:     s.materializeFallbacks,
		OverlayCapacityFallbacks: s.overlayCapacityFallbacks,
		OverlayErrorFallbacks:    s.overlayErrorFallbacks,
		OverlayLastFallback:      s.overlayLastFallback,
		MaterializeCopyErrors:    s.materializeCopyErrors,
		MaterializeCopyDuration:  s.materializeCopyDuration,
		MaterializeCapacity:      cap(s.ioSlots),
		MaterializeActive:        s.materializeActive,
		MaterializePeakActive:    s.materializePeakActive,
		MaterializeWaitDuration:  s.materializeWaitDuration,
		OverlayAvailable:         s.overlay != nil,
		OverlayBackend:           overlayBackend,
		OverlayActive:            s.overlayActive(),
		OverlayCapacity:          cap(s.overlaySlots),
		AbsorbFastPaths:          s.absorbFastPaths,
		AbsorbUpperAttempts:      s.absorbUpperAttempts,
		AbsorbUpperEntries:       s.absorbUpperEntries,
		AbsorbUpperFallbacks:     s.absorbUpperFallbacks,
		AbsorbUpperErrors:        s.absorbUpperErrors,
		AbsorbUpperDuration:      s.absorbUpperDuration,
		AbsorbScans:              s.absorbScans,
		AbsorbScanErrors:         s.absorbScanErrors,
		AbsorbScanDuration:       s.absorbScanDuration,
		AbsorbContentComparisons: s.absorbContentComparisons,
		AbsorbCapacity:           cap(s.ioSlots),
		AbsorbActive:             s.absorbActive,
		AbsorbPeakActive:         s.absorbPeakActive,
		AbsorbWaitDuration:       s.absorbWaitDuration,
		Handoffs:                 s.handoffs,
		PublishAttempts:          s.publishAttempts,
		PublishCommits:           s.publishCommits,
		PublishErrors:            s.publishErrors,
		PublishCleanupErrors:     s.publishCleanupErrors,
		PublishDuration:          s.publishDuration,
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
// 物化是惰性的：只有命令执行或显式 Materialize 才把可见树落到 live 目录。
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
	s.mu.Unlock()
	// 惰性物化：Fork 只建 overlay，不拷贝基线树。live 目录在第一次需要时
	// （首条命令、join 准备等）由 Materialize 按需创建；纯认知型环境永不落盘。
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
		delete(s.lives, parentID)
		s.lives[childID] = live
		delete(s.liveBaselines, parentID)
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
		if s.overlayStateExists(parentID) {
			if err := s.closeOverlay(parentID); err != nil {
				s.mu.Unlock()
				return err
			}
			parentState := s.overlayStatePath(parentID)
			childState := s.overlayStatePath(childID)
			if err := os.Rename(parentState, childState); err != nil {
				_, _, restoreErr := s.persistedLive(parentID)
				s.mu.Unlock()
				return errors.Join(
					fmt.Errorf("vfs: handoff overlay state: %w", err),
					restoreErr,
				)
			}
			if err := os.Rename(live, childLive); err != nil {
				rollbackErr := os.Rename(childState, parentState)
				_, _, restoreErr := s.persistedLive(parentID)
				s.mu.Unlock()
				return errors.Join(
					fmt.Errorf("vfs: handoff overlay mountpoint: %w", err),
					rollbackErr,
					restoreErr,
				)
			}
			remounted, ok, err := s.persistedLive(childID)
			if err != nil || !ok {
				s.mu.Unlock()
				return errors.Join(
					fmt.Errorf("vfs: remount handed-off overlay"),
					err,
				)
			}
			childLive = remounted
		} else if err := os.Rename(live, childLive); err != nil {
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
	if baseline, ok := s.liveBaselines[parentID]; ok {
		s.liveBaselines[childID] = baseline
	}
	delete(s.liveBaselines, parentID)
	s.handoffs++
	s.mu.Unlock()
	return nil
}

func (s *Store) persistedLive(envID string) (string, bool, error) {
	if s.liveRoot == "" || envID == "" {
		return "", false, nil
	}
	if live, ok, err := s.restoreOverlay(envID); ok || err != nil {
		return live, ok, err
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

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
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
// 持久 store 上首次写入会触发物化：live 目录是持久层，纯 overlay 写入重启即丢。
func (v *View) Write(path string, data []byte) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	if live, ok := v.liveDir(); ok {
		return writeLive(live, rel, data)
	}
	if v.store.liveRoot != "" {
		if _, err := v.store.Materialize(v.envID); err != nil {
			return err
		}
		if live, ok := v.liveDir(); ok {
			return writeLive(live, rel, data)
		}
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

// Delete 在本环境 overlay 上打 tombstone；已物化则删 live。持久 store 上首次删除同样触发物化。
func (v *View) Delete(path string) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	if live, ok := v.liveDir(); ok {
		return deleteLive(live, rel)
	}
	if v.store.liveRoot != "" {
		if _, err := v.store.Materialize(v.envID); err != nil {
			return err
		}
		if live, ok := v.liveDir(); ok {
			return deleteLive(live, rel)
		}
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
	root, err := confinedRoot(s.floorDir)
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

func confinedRoot(floorDir string) (string, error) {
	abs, err := filepath.Abs(floorDir)
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
