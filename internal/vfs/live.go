package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Materialize 把 env 的可见树落到 live 目录。已物化则原样返回。
func (s *Store) Materialize(envID string) (live string, retErr error) {
	s.mu.Lock()
	if dir, ok := s.lives[envID]; ok {
		s.mu.Unlock()
		return dir, nil
	}
	if call := s.materializing[envID]; call != nil {
		s.mu.Unlock()
		<-call.done
		return call.dir, call.err
	}
	call := &materializeCall{done: make(chan struct{})}
	s.materializing[envID] = call
	defer func() {
		s.mu.Lock()
		call.dir, call.err = live, retErr
		delete(s.materializing, envID)
		close(call.done)
		s.mu.Unlock()
	}()
	base, err := confinedRoot(s.floorDir)
	if err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("vfs: materialize: %w", err)
	}
	blobs := s.overlayBlobs(envID)
	s.mu.Unlock()
	s.beginMaterializeIO()
	defer s.endMaterializeIO()

	if s.liveRoot != "" {
		if restored, ok, restoreErr := s.persistedLive(envID); restoreErr != nil {
			return "", restoreErr
		} else if ok {
			s.mu.Lock()
			s.lives[envID] = restored
			s.mu.Unlock()
			return restored, nil
		}
	}
	materializeStarted := time.Now()
	if overlayLive, ok, overlayErr := s.createOverlay(envID, base, blobs); ok {
		live = overlayLive
		var baseline *liveFingerprint
		if _, mounted := s.overlayUpper(envID); !mounted {
			baseline = scanLiveFingerprint(live)
		}
		s.mu.Lock()
		s.materializeCopies++
		s.materializeOverlays++
		s.materializeCopyDuration += time.Since(materializeStarted)
		s.lives[envID] = live
		if baseline != nil {
			s.liveBaselines[envID] = baseline
		}
		s.mu.Unlock()
		return live, nil
	} else if overlayErr != nil {
		s.mu.Lock()
		s.materializeFallbacks++
		if errors.Is(overlayErr, errOverlayCapacity) {
			s.overlayCapacityFallbacks++
		} else {
			s.overlayErrorFallbacks++
		}
		s.overlayLastFallback = overlayErr.Error()
		s.mu.Unlock()
		if errors.Is(overlayErr, errOverlayCleanup) || s.overlayStateExists(envID) {
			return "", overlayErr
		}
	}

	if s.liveRoot == "" {
		live, err = os.MkdirTemp("", "threadmill-live-")
	} else {
		live, err = os.MkdirTemp(s.liveRoot, ".tmp-")
	}
	if err != nil {
		return "", fmt.Errorf("vfs: materialize: %w", err)
	}
	copyStarted := time.Now()
	copyBackend, copyErr := copyTree(base, live)
	s.mu.Lock()
	s.materializeCopies++
	s.materializeReflinks += boolCount(copyBackend == materializeReflink)
	s.materializeFullCopies += boolCount(copyBackend == materializeFullCopy)
	s.materializeCopyDuration += time.Since(copyStarted)
	if copyErr != nil {
		s.materializeCopyErrors++
	}
	s.mu.Unlock()
	if copyErr != nil {
		os.RemoveAll(live)
		return "", copyErr
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

	baseline := scanLiveFingerprint(live)
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.lives[envID]; ok {
		os.RemoveAll(live)
		return dir, nil
	}
	s.lives[envID] = live
	s.liveBaselines[envID] = baseline
	return live, nil
}

// Absorb 把 live 相对 overlay+host 的增量写回 overlay。未物化或 envID 为空则是空操作。
func (s *Store) Absorb(envID string) error {
	if envID == "" {
		return nil
	}
	s.mu.Lock()
	live, ok := s.lives[envID]
	baseline, hasBaseline := s.liveBaselines[envID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	s.beginAbsorbIO()
	defer s.endAbsorbIO()
	upperStarted := time.Now()
	attempted, used, upperEntries, upperErr := s.absorbOverlayUpper(envID, live)
	if attempted {
		s.mu.Lock()
		s.absorbUpperAttempts++
		s.absorbUpperEntries += upperEntries
		s.absorbUpperDuration += time.Since(upperStarted)
		if !used {
			s.absorbUpperFallbacks++
		}
		if upperErr != nil {
			s.absorbUpperErrors++
		}
		if used {
			s.absorbFastPaths++
		}
		s.mu.Unlock()
		if used {
			if upperErr != nil {
				return fmt.Errorf("vfs: absorb upperdir: %w", upperErr)
			}
			return nil
		}
	}
	current := scanLiveFingerprint(live)
	if hasBaseline && current.valid && current.hash == baseline.hash {
		s.mu.Lock()
		s.absorbFastPaths++
		s.mu.Unlock()
		return nil
	}
	scanStarted := time.Now()

	s.mu.Lock()
	if _, ok := s.lives[envID]; !ok {
		s.mu.Unlock()
		s.recordAbsorbScan(scanStarted, nil)
		return nil
	}
	beforeFiles, err := s.visibleRegularFiles(envID)
	var beforeDirectories map[string]struct{}
	if err == nil {
		beforeDirectories, err = s.visibleDirectories(envID)
	}
	s.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("vfs: absorb: %w", err)
		s.recordAbsorbScan(scanStarted, err)
		return err
	}
	liveFiles, liveDirectories, comparisons, err := walkRegularFiles(
		live,
		beforeFiles,
		beforeDirectories,
		unchangedLiveBuckets(baseline, current),
	)
	s.mu.Lock()
	s.absorbContentComparisons += comparisons
	s.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("vfs: absorb: %w", err)
		s.recordAbsorbScan(scanStarted, err)
		return err
	}
	s.mu.Lock()
	if _, ok := s.lives[envID]; !ok {
		s.mu.Unlock()
		s.recordAbsorbScan(scanStarted, nil)
		return nil
	}
	dst := s.ensure(envID)
	deletedDirectories := shallowMissingDirectories(beforeDirectories, liveDirectories)
	deletedDirectorySet := make(map[string]struct{}, len(deletedDirectories))
	for _, path := range deletedDirectories {
		deletedDirectorySet[path] = struct{}{}
		applyBlob(dst, path, blob{tombstone: true})
	}
	for path := range beforeFiles {
		if hasPathAncestor(deletedDirectorySet, path) {
			continue
		}
		if _, ok := liveFiles[path]; !ok {
			applyBlob(dst, path, blob{tombstone: true})
		}
	}
	for path, current := range liveFiles {
		old, existed := beforeFiles[path]
		if existed && snapshotsEqual(old, current) {
			continue
		}
		applyBlob(dst, path, blob{
			data:       current.data,
			executable: current.executable,
		})
	}
	s.liveBaselines[envID] = current
	s.mu.Unlock()
	s.recordAbsorbScan(scanStarted, nil)
	return nil
}

func (s *Store) beginMaterializeIO() {
	waitStarted := time.Now()
	s.ioSlots <- struct{}{}
	s.mu.Lock()
	s.materializeWaitDuration += time.Since(waitStarted)
	s.materializeActive++
	s.materializePeakActive = max(s.materializePeakActive, s.materializeActive)
	s.mu.Unlock()
}

func (s *Store) endMaterializeIO() {
	s.mu.Lock()
	s.materializeActive--
	s.mu.Unlock()
	<-s.ioSlots
}

func (s *Store) beginAbsorbIO() {
	waitStarted := time.Now()
	s.ioSlots <- struct{}{}
	s.mu.Lock()
	s.absorbWaitDuration += time.Since(waitStarted)
	s.absorbActive++
	s.absorbPeakActive = max(s.absorbPeakActive, s.absorbActive)
	s.mu.Unlock()
}

func (s *Store) endAbsorbIO() {
	s.mu.Lock()
	s.absorbActive--
	s.mu.Unlock()
	<-s.ioSlots
}

func (s *Store) recordAbsorbScan(start time.Time, err error) {
	s.mu.Lock()
	s.absorbScans++
	s.absorbScanDuration += time.Since(start)
	if err != nil {
		s.absorbScanErrors++
	}
	s.mu.Unlock()
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
		delete(s.liveBaselines, envID)
	}
	s.mu.Unlock()
	if !ok {
		return aerr
	}
	if err := s.closeOverlay(envID); err != nil {
		return errors.Join(aerr, err)
	}
	if err := os.RemoveAll(live); err != nil {
		return errors.Join(aerr, fmt.Errorf("vfs: release: %w", err))
	}
	return aerr
}

// Freeze absorbs an environment and releases its live workspace while keeping
// the logical snapshot available for later reads or publication. Persistent
// copy/reflink workspaces stay on disk because they are the snapshot; persistent
// OverlayFS workspaces keep only their upper/work state after unmounting.
func (s *Store) Freeze(envID string) error {
	if envID == "" {
		return nil
	}
	if err := s.Absorb(envID); err != nil {
		return err
	}
	s.mu.Lock()
	live := s.lives[envID]
	s.mu.Unlock()
	if live == "" {
		return nil
	}
	persistentOverlay := s.overlayStateExists(envID)
	if err := s.closeOverlay(envID); err != nil {
		return err
	}
	if s.liveRoot != "" && !persistentOverlay {
		return nil
	}
	s.mu.Lock()
	delete(s.lives, envID)
	delete(s.liveBaselines, envID)
	s.mu.Unlock()
	if err := os.RemoveAll(live); err != nil {
		return fmt.Errorf("vfs: freeze: %w", err)
	}
	return nil
}

// Archive records sourceID under archiveID without changing the source. The
// archive is persisted when the store has a live root, so it survives manager
// restarts after the source task workspace has been handed off or discarded.
func (s *Store) Archive(sourceID, archiveID string) error {
	if sourceID == "" || archiveID == "" || sourceID == archiveID {
		return fmt.Errorf("vfs: archive: %w", ErrInvalidPath)
	}
	if err := s.Restore(sourceID); err != nil {
		return fmt.Errorf("vfs: archive source: %w", err)
	}
	if err := s.Discard(archiveID); err != nil {
		return fmt.Errorf("vfs: replace archive: %w", err)
	}
	if err := s.Fork(sourceID, archiveID); err != nil {
		return fmt.Errorf("vfs: fork archive: %w", err)
	}
	if s.liveRoot == "" {
		return nil
	}
	if _, err := s.Materialize(archiveID); err != nil {
		return errors.Join(err, s.Discard(archiveID))
	}
	if err := s.Freeze(archiveID); err != nil {
		return fmt.Errorf("vfs: freeze archive: %w", err)
	}
	return nil
}

// Restore opens an existing in-memory or persistent environment without
// silently creating an empty one.
func (s *Store) Restore(envID string) error {
	if envID == "" {
		return ErrUnknownEnvironment
	}
	s.mu.Lock()
	_, envOK := s.envs[envID]
	_, liveOK := s.lives[envID]
	s.mu.Unlock()
	if envOK || liveOK {
		return nil
	}
	if s.liveRoot == "" {
		return fmt.Errorf("%w: %s", ErrUnknownEnvironment, envID)
	}
	live, ok, err := s.persistedLive(envID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownEnvironment, envID)
	}
	s.mu.Lock()
	s.envs[envID] = &layer{files: make(map[string]blob)}
	s.lives[envID] = live
	s.mu.Unlock()
	return nil
}

// Discard 删除 env 的 live 目录和 overlay，不吸收尚未收回的写入。
func (s *Store) Discard(envID string) error {
	if envID == "" {
		return nil
	}
	s.mu.Lock()
	live := s.lives[envID]
	s.mu.Unlock()
	if live == "" && s.liveRoot != "" {
		live = s.persistentLivePath(envID)
	}
	if err := s.closeOverlay(envID); err != nil {
		return err
	}
	if live != "" {
		if err := os.RemoveAll(live); err != nil {
			return fmt.Errorf("vfs: discard: %w", err)
		}
	}
	if s.liveRoot != "" {
		if err := os.RemoveAll(s.overlayStatePath(envID)); err != nil {
			return fmt.Errorf("vfs: discard overlay state: %w", err)
		}
	}
	s.mu.Lock()
	delete(s.lives, envID)
	delete(s.liveBaselines, envID)
	delete(s.envs, envID)
	s.mu.Unlock()
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

type fileSnapshot struct {
	data       []byte
	source     string
	sourceInfo fs.FileInfo
	executable bool
}

// walkRegularFiles 扫 live 树并读出每个变更候选的内容。
// overlay 只存路径到字节（和删除标记），不跟踪 live inode。Absorb 用这份
// 内容和 overlay+host 的可见树做比对，只把增删改写回 overlay，所以必须读字节，
// 不能只看文件名。
// 未改变的大型 base 文件通过内容摘要确认，不进入 overlay 限额。
func walkRegularFiles(
	root string,
	before map[string]fileSnapshot,
	beforeDirectories map[string]struct{},
	unchangedBuckets [liveFingerprintBuckets]bool,
) (map[string]fileSnapshot, map[string]struct{}, uint64, error) {
	out := map[string]fileSnapshot{}
	directories := map[string]struct{}{}
	var totalSize int64
	var contentComparisons uint64
	compareA := make([]byte, 32*1024)
	compareB := make([]byte, len(compareA))
	ignored := gitIgnoredPaths(root)

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
		rel = filepath.ToSlash(rel)
		if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(root, path) {
			return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
		}
		if directory, ok := ignored[rel]; ok {
			retainIgnoredBase(out, before, rel)
			retainIgnoredDirectories(directories, beforeDirectories, rel)
			if directory && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		mode := d.Type()
		if d.IsDir() {
			directories[rel] = struct{}{}
			return nil
		}
		if mode&os.ModeSymlink != 0 {
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
		executable := info.Mode().Perm()&0o111 != 0
		old, existed := before[rel]
		if existed && unchangedBuckets[liveFingerprintBucket(rel)] {
			out[rel] = old
			return nil
		}
		if existed && old.source != "" {
			sourceInfo := old.sourceInfo
			if sourceInfo == nil {
				var err error
				sourceInfo, err = os.Stat(old.source)
				if err != nil {
					return err
				}
			}
			old.executable = sourceInfo.Mode().Perm()&0o111 != 0
			if old.executable == executable {
				contentComparisons++
				equal, err := equalFileContents(
					path,
					info,
					old.source,
					sourceInfo,
					compareA,
					compareB,
				)
				if err != nil {
					return err
				}
				if equal {
					out[rel] = old
					return nil
				}
			}
		}
		if info.Size() > MaxFileSize {
			return fmt.Errorf("%w: %q (%d > %d)", ErrFileTooLarge, rel, info.Size(), MaxFileSize)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		current := fileSnapshot{data: data, executable: executable}
		out[rel] = current
		if existed && snapshotsEqual(old, current) {
			return nil
		}
		totalSize += info.Size()
		if totalSize > MaxTotalSize {
			return fmt.Errorf("%w: limit %d", ErrTotalSizeExceeded, MaxTotalSize)
		}
		return nil
	})
	if err != nil {
		return nil, nil, contentComparisons, err
	}
	return out, directories, contentComparisons, nil
}

func gitIgnoredPaths(root string) map[string]bool {
	cmd := osexec.Command(
		"git",
		"-c", "core.fsmonitor=false",
		"-c", "core.excludesFile="+os.DevNull,
		"-C", root,
		"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	ignored := make(map[string]bool)
	for _, item := range bytes.Split(output, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		directory := item[len(item)-1] == '/'
		rel := filepath.ToSlash(strings.TrimSuffix(string(item), "/"))
		if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			continue
		}
		ignored[rel] = directory
	}
	return ignored
}

func retainIgnoredBase(
	out, before map[string]fileSnapshot,
	rel string,
) {
	if old, ok := before[rel]; ok && old.source != "" {
		out[rel] = old
	}
	prefix := rel + "/"
	for path, old := range before {
		if old.source != "" && strings.HasPrefix(path, prefix) {
			out[path] = old
		}
	}
}

func retainIgnoredDirectories(
	out, before map[string]struct{},
	rel string,
) {
	if _, ok := before[rel]; ok {
		out[rel] = struct{}{}
	}
	prefix := rel + "/"
	for path := range before {
		if strings.HasPrefix(path, prefix) {
			out[path] = struct{}{}
		}
	}
}

func (s *Store) visibleRegularFiles(envID string) (map[string]fileSnapshot, error) {
	baseFiles, err := s.cachedBaseRegularFiles()
	if err != nil {
		return nil, err
	}
	out := make(map[string]fileSnapshot, len(baseFiles))
	for path, snapshot := range baseFiles {
		out[path] = snapshot
	}

	// Most paths come straight from the read-only base. Only paths touched or
	// masked by an overlay need the full overlay lookup and content clone.
	overlayPaths := map[string]struct{}{}
	overlayParents := map[string]struct{}{}
	for _, files := range s.overlayMaps(envID) {
		for path := range files {
			overlayPaths[path] = struct{}{}
			for parent := path; ; {
				i := strings.LastIndex(parent, "/")
				if i <= 0 {
					break
				}
				parent = parent[:i]
				overlayParents[parent] = struct{}{}
			}
		}
	}
	candidates := make(map[string]struct{}, len(overlayPaths))
	for path := range overlayPaths {
		candidates[path] = struct{}{}
	}
	for path := range out {
		_, exact := overlayPaths[path]
		_, replacedByDir := overlayParents[path]
		if !exact && !replacedByDir && !hasPathAncestor(overlayPaths, path) {
			continue
		}
		delete(out, path)
		candidates[path] = struct{}{}
	}
	for path := range candidates {
		snapshot, ok := s.regularFileSnapshot(envID, path)
		if ok {
			out[path] = snapshot
		}
	}
	return out, nil
}

func (s *Store) cachedBaseRegularFiles() (map[string]fileSnapshot, error) {
	s.baseFilesOnce.Do(func() {
		base, err := confinedRoot(s.floorDir)
		if err != nil {
			s.baseFilesErr = err
			return
		}
		files := make(map[string]fileSnapshot)
		directories := make(map[string]struct{})
		s.baseFilesErr = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, path)
			if err != nil || rel == "." {
				return err
			}
			rel = filepath.ToSlash(rel)
			if filepath.IsAbs(rel) || !filepath.IsLocal(rel) || escapesRoot(base, path) {
				return fmt.Errorf("%w: %q", ErrInvalidPath, rel)
			}
			mode := d.Type()
			if d.IsDir() {
				directories[rel] = struct{}{}
				return nil
			}
			if mode&os.ModeSymlink != 0 {
				return nil
			}
			if mode.Type() != 0 {
				return fmt.Errorf("%w: %q (mode %s)", ErrSpecialFile, rel, mode.String())
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			files[rel] = fileSnapshot{
				source:     path,
				sourceInfo: info,
				executable: info.Mode().Perm()&0o111 != 0,
			}
			return nil
		})
		if s.baseFilesErr == nil {
			s.baseFiles = files
			s.baseDirs = directories
		}
	})
	return s.baseFiles, s.baseFilesErr
}

func (s *Store) visibleDirectories(envID string) (map[string]struct{}, error) {
	if _, err := s.cachedBaseRegularFiles(); err != nil {
		return nil, err
	}
	candidates := make(map[string]struct{}, len(s.baseDirs))
	for path := range s.baseDirs {
		candidates[path] = struct{}{}
	}
	for _, files := range s.overlayMaps(envID) {
		for path := range files {
			candidates[path] = struct{}{}
			for parent := path; ; {
				i := strings.LastIndex(parent, "/")
				if i <= 0 {
					break
				}
				parent = parent[:i]
				candidates[parent] = struct{}{}
			}
		}
	}

	visible := make(map[string]struct{}, len(candidates))
	for path := range candidates {
		info, handled, statErr := s.lookupStat(envID, path)
		if handled {
			if statErr == nil && info.IsDir {
				visible[path] = struct{}{}
			}
			continue
		}
		if _, ok := s.baseDirs[path]; ok {
			visible[path] = struct{}{}
		}
	}
	return visible, nil
}

func shallowMissingDirectories(
	before, after map[string]struct{},
) []string {
	missing := make([]string, 0)
	for path := range before {
		if _, ok := after[path]; !ok {
			missing = append(missing, path)
		}
	}
	slices.Sort(missing)
	selected := make(map[string]struct{}, len(missing))
	shallow := missing[:0]
	for _, path := range missing {
		if hasPathAncestor(selected, path) {
			continue
		}
		selected[path] = struct{}{}
		shallow = append(shallow, path)
	}
	return shallow
}

func hasPathAncestor(paths map[string]struct{}, rel string) bool {
	for {
		i := strings.LastIndex(rel, "/")
		if i <= 0 {
			return false
		}
		rel = rel[:i]
		if _, ok := paths[rel]; ok {
			return true
		}
	}
}

func (s *Store) regularFileSnapshot(envID, rel string) (fileSnapshot, bool) {
	b, found := s.lookupBlobValue(envID, rel)
	if found {
		if b.tombstone {
			return fileSnapshot{}, false
		}
		return fileSnapshot{
			data:       cloneBytes(b.data),
			executable: b.executable,
		}, true
	}
	if s.hasOverlayChildren(envID, rel) {
		return fileSnapshot{}, false
	}
	host, err := s.resolveHost(rel)
	if err != nil {
		return fileSnapshot{}, false
	}
	fi, err := os.Stat(host)
	if err != nil || fi.IsDir() || fi.Mode().Type() != 0 {
		return fileSnapshot{}, false
	}
	return fileSnapshot{
		source:     host,
		sourceInfo: fi,
		executable: fi.Mode().Perm()&0o111 != 0,
	}, true
}

func snapshotsEqual(a, b fileSnapshot) bool {
	if a.executable != b.executable {
		return false
	}
	if a.source != "" || b.source != "" {
		return a.source != "" && b.source != ""
	}
	return bytes.Equal(a.data, b.data)
}

func equalFileContents(
	a string,
	aInfo fs.FileInfo,
	b string,
	bInfo fs.FileInfo,
	aBuf, bBuf []byte,
) (equal bool, err error) {
	if aInfo.Size() != bInfo.Size() {
		return false, nil
	}
	aFile, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, aFile.Close())
	}()
	bFile, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, bFile.Close())
	}()
	for {
		aRead, aErr := io.ReadFull(aFile, aBuf)
		bRead, bErr := io.ReadFull(bFile, bBuf)
		if aRead != bRead || !bytes.Equal(aBuf[:aRead], bBuf[:bRead]) {
			return false, nil
		}
		if aErr == nil && bErr == nil {
			continue
		}
		if (errors.Is(aErr, io.EOF) || errors.Is(aErr, io.ErrUnexpectedEOF)) &&
			(errors.Is(bErr, io.EOF) || errors.Is(bErr, io.ErrUnexpectedEOF)) {
			return true, nil
		}
		return false, errors.Join(aErr, bErr)
	}
}

func applyLive(live, rel string, b blob) error {
	if b.tombstone {
		return deleteLive(live, rel)
	}
	return writeLiveMode(live, rel, b.data, b.executable)
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
	executable := false
	if info, statErr := os.Lstat(dest); statErr == nil {
		executable = info.Mode().Type() == 0 && info.Mode().Perm()&0o111 != 0
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	return writeLiveModeAt(dest, data, executable)
}

func writeLiveMode(live, rel string, data []byte, executable bool) error {
	dest, err := createLivePath(live, rel)
	if err != nil {
		return err
	}
	return writeLiveModeAt(dest, data, executable)
}

func writeLiveModeAt(dest string, data []byte, executable bool) error {
	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, data, regularMode(executable))
}

func regularMode(executable bool) fs.FileMode {
	if executable {
		return 0o750
	}
	return 0o640
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

type materializeCopyBackend uint8

const (
	materializeFullCopy materializeCopyBackend = iota
	materializeReflink
)

func copyTree(src, dst string) (materializeCopyBackend, error) {
	clone := osexec.Command("cp", "--reflink=always", "-a", src+"/.", dst)
	if err := clone.Run(); err == nil {
		return materializeReflink, nil
	}
	if err := resetCopyDestination(dst); err != nil {
		return materializeFullCopy, err
	}
	copyCmd := osexec.Command("cp", "--reflink=never", "-a", src+"/.", dst)
	if err := copyCmd.Run(); err == nil {
		return materializeFullCopy, nil
	}
	if err := resetCopyDestination(dst); err != nil {
		return materializeFullCopy, err
	}
	return materializeFullCopy, copyWalk(src, dst)
}

func resetCopyDestination(dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("vfs: reset materialization: %w", err)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return fmt.Errorf("vfs: reset materialization: %w", err)
	}
	return nil
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
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(
			target,
			data,
			regularMode(info.Mode().Perm()&0o111 != 0),
		)
	})
}

func boolCount(ok bool) uint64 {
	if ok {
		return 1
	}
	return 0
}
