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
	"slices"
	"strings"
	"sync"
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
	data      []byte
	tombstone bool
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
}

// Stats 是 VFS 当前持有的有界资源清单。
type Stats struct {
	Environments int   `json:"environments"`
	LiveDirs     int   `json:"live_dirs"`
	OverlayFiles int   `json:"overlay_files"`
	Tombstones   int   `json:"tombstones"`
	OverlayBytes int64 `json:"overlay_bytes"`
}

// NewStore 以只读 host 树为 base。写入不会改 baseDir。
func NewStore(baseDir string) *Store {
	return &Store{
		baseDir: baseDir,
		envs:    make(map[string]*layer),
		lives:   make(map[string]string),
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
		Environments: len(s.envs),
		LiveDirs:     len(s.lives),
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

	var childFiles map[string]blob
	var fromLayer *layer
	parentID := ""
	if l, ok := s.envs[from]; ok {
		fromLayer = l
		childFiles = l.files
		parentID = l.parentID
	}

	type pending struct {
		path string
		b    blob
	}
	var apply []pending
	var baseline []map[string]blob
	if fromLayer != nil {
		baseline = fromLayer.baseline
	}
	for path, b := range childFiles {
		if b.tombstone {
			continue
		}
		prefix := path + "/"
		for other, ob := range childFiles {
			if ob.tombstone || other == path || !strings.HasPrefix(other, prefix) {
				continue
			}
			return fmt.Errorf("vfs: merge conflict: %s", other)
		}
	}
	for path, theirsBlob := range childFiles {
		theirs := overlayContent(theirsBlob)
		base := s.mergeBase(fromLayer, parentID, path)
		ours := s.lookupContent(into, path)
		sameAncestor := contentEqual(theirs, base) || contentEqual(ours, theirs)
		if !sameAncestor && !contentEqual(ours, base) {
			return fmt.Errorf("vfs: merge conflict: %s", path)
		}
		needApply := !sameAncestor
		for _, q := range s.knownDescendants(into, baseline, path) {
			tq := s.lookupContent(from, q)
			bq := s.mergeBase(fromLayer, parentID, q)
			oq := s.lookupContent(into, q)
			if contentEqual(tq, bq) || contentEqual(oq, tq) {
				continue
			}
			if !contentEqual(oq, bq) {
				return fmt.Errorf("vfs: merge conflict: %s", q)
			}
			needApply = true
		}
		if !needApply {
			continue
		}
		if !theirs.tombstone && s.liveFileAncestor(into, path) {
			return fmt.Errorf("vfs: merge conflict: %s", path)
		}
		apply = append(apply, pending{path: path, b: cloneBlob(theirsBlob)})
	}
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

type content struct {
	exists    bool
	tombstone bool
	data      []byte
	maskFrom  string
	maskFile  bool
}

func overlayContent(b blob) content {
	return content{exists: true, tombstone: b.tombstone, data: b.data}
}

func maskedContent(prefix string, b blob) content {
	c := content{exists: true, tombstone: true, maskFrom: prefix, maskFile: !b.tombstone}
	if !b.tombstone {
		c.data = cloneBytes(b.data)
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
	data, err := os.ReadFile(host)
	if err != nil {
		return content{}
	}
	return content{exists: true, data: data}
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
			return bytes.Equal(a.data, b.data)
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
	return bytes.Equal(a.data, b.data)
}

func exactHidden(c content) bool {
	return !c.exists || c.tombstone
}

func cloneBlob(b blob) blob {
	if b.tombstone {
		return blob{tombstone: true}
	}
	return blob{data: cloneBytes(b.data)}
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
	v.store.ensure(v.envID).files[rel] = blob{data: cloneBytes(data)}
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
	for _, files := range s.overlayMaps(envID) {
		if b, ok := files[rel]; ok {
			return b.data, b.tombstone, true
		}
		if _, ok := filesMask(files, rel); ok {
			return nil, true, true
		}
	}
	return nil, false, false
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
