// Package vfs 为每个环境提供逻辑 overlay 文件视图。
// 写只落在本环境 overlay；读沿 overlay → parent → 只读 base。
// FileInfo 与 DirEnt 是本包本地类型（字段与 env 对齐），等 Env.Files 接线后再适配。
package vfs

import (
	"bytes"
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
var ErrInvalidPath = errors.New("vfs: invalid path")

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

// Store 按环境保存 overlay。Fork 只挂 parent 指针，不复制树。
type Store struct {
	mu      sync.Mutex // ponytail: one store mutex, per-env locks if throughput matters
	baseDir string
	envs    map[string]*layer
}

// NewStore 以只读 host 树为 base。写入不会改 baseDir。
func NewStore(baseDir string) *Store {
	return &Store{
		baseDir: baseDir,
		envs:    make(map[string]*layer),
	}
}

// Fork 给 child 挂上 parent 指针和空 overlay，并记下当时从父到根的 overlay 作为合入基线。
// 子环境已存在时不覆盖，也不改基线。
func (s *Store) Fork(parentID, childID string) {
	if childID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.envs[childID]; exists {
		return
	}
	s.envs[childID] = &layer{
		parentID: parentID,
		files:    make(map[string]blob),
		baseline: s.snapshotOverlays(parentID),
	}
}

// Merge 把 from 的 overlay 增量三路并入 into。冲突失败，不改 into。
func (s *Store) Merge(from, into string) error {
	if into == "" {
		return nil
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
	for path, theirsBlob := range childFiles {
		theirs := overlayContent(theirsBlob)
		base := s.mergeBase(fromLayer, parentID, path)
		ours := s.lookupContent(into, path)
		if contentEqual(theirs, base) || contentEqual(ours, theirs) {
			continue
		}
		if !contentEqual(ours, base) {
			return fmt.Errorf("vfs: merge conflict: %s", path)
		}
		for _, q := range s.knownDescendants(into, baseline, path) {
			if !contentEqual(s.lookupContent(into, q), s.mergeBase(fromLayer, parentID, q)) {
				return fmt.Errorf("vfs: merge conflict: %s", q)
			}
		}
		apply = append(apply, pending{path: path, b: cloneBlob(theirsBlob)})
	}
	if len(apply) == 0 {
		return nil
	}
	dst := s.ensure(into)
	for _, e := range apply {
		applyBlob(dst, e.path, e.b)
	}
	return nil
}

type content struct {
	exists    bool
	tombstone bool
	data      []byte
}

func overlayContent(b blob) content {
	return content{exists: true, tombstone: b.tombstone, data: b.data}
}

func (s *Store) mergeBase(fromLayer *layer, parentID, rel string) content {
	if fromLayer != nil && fromLayer.baseline != nil {
		return s.lookupFrozen(fromLayer.baseline, rel)
	}
	return s.lookupContent(parentID, rel)
}

func (s *Store) snapshotOverlays(envID string) []map[string]blob {
	out := []map[string]blob{}
	seen := map[string]struct{}{}
	for id := envID; id != ""; {
		if _, loop := seen[id]; loop {
			break
		}
		seen[id] = struct{}{}
		l, ok := s.envs[id]
		if !ok {
			break
		}
		out = append(out, cloneFiles(l.files))
		id = l.parentID
	}
	return out
}

func (s *Store) lookupFrozen(chain []map[string]blob, rel string) content {
	for _, files := range chain {
		if b, ok := files[rel]; ok {
			return overlayContent(b)
		}
		if filesMask(files, rel) {
			return content{exists: true, tombstone: true}
		}
	}
	return s.lookupHost(rel)
}

func (s *Store) lookupContent(envID, rel string) content {
	seen := map[string]struct{}{}
	for id := envID; id != ""; {
		if _, loop := seen[id]; loop {
			break
		}
		seen[id] = struct{}{}
		l, ok := s.envs[id]
		if !ok {
			break
		}
		if b, ok := l.files[rel]; ok {
			return overlayContent(b)
		}
		if layerMasks(l, rel) {
			return content{exists: true, tombstone: true}
		}
		id = l.parentID
	}
	return s.lookupHost(rel)
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
	for _, l := range s.chain(into) {
		add(l.files)
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
		prefix := path + "/"
		for k := range dst.files {
			if strings.HasPrefix(k, prefix) {
				delete(dst.files, k)
			}
		}
		return
	}
	for _, ancestor := range ancestorPrefixes(path) {
		if existing, ok := dst.files[ancestor]; ok && existing.tombstone {
			delete(dst.files, ancestor)
		}
	}
	dst.files[path] = cloneBlob(b)
}

func contentEqual(a, b content) bool {
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

// Read 沿 overlay → parent → base 读文件，返回拷贝。
func (v *View) Read(path string) ([]byte, error) {
	rel, err := jail(path)
	if err != nil {
		return nil, err
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()

	if data, tombstone, found := v.store.lookupBlob(v.envID, rel); found {
		if tombstone {
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

// Write 只写入本环境 overlay。
func (v *View) Write(path string, data []byte) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	v.store.ensure(v.envID).files[rel] = blob{data: cloneBytes(data)}
	return nil
}

// Delete 在本环境 overlay 上打 tombstone，隐藏 parent/base 同名文件。
func (v *View) Delete(path string) error {
	rel, err := jail(path)
	if err != nil {
		return err
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	v.store.ensure(v.envID).files[rel] = blob{tombstone: true}
	return nil
}

// Stat 沿 overlay → parent → base 返回元数据。
func (v *View) Stat(path string) (FileInfo, error) {
	rel, err := jail(path)
	if err != nil {
		return FileInfo{}, err
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

// List 合并 base 目录项与 overlay（tombstone 会去掉对应名字）。
func (v *View) List(path string) ([]DirEnt, error) {
	rel, err := jail(path)
	if err != nil {
		return nil, err
	}
	v.store.mu.Lock()
	defer v.store.mu.Unlock()

	if _, tombstone, found := v.store.lookupBlob(v.envID, rel); found {
		if tombstone {
			return nil, notFound(rel)
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

	names := make([]string, 0, len(ents))
	for name := range ents {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]DirEnt, 0, len(names))
	for _, name := range names {
		out = append(out, ents[name])
	}
	return out, nil
}

func (s *Store) ensure(envID string) *layer {
	if l, ok := s.envs[envID]; ok {
		return l
	}
	l := &layer{files: make(map[string]blob)}
	s.envs[envID] = l
	return l
}

func (s *Store) chain(envID string) []*layer {
	var out []*layer
	seen := map[string]struct{}{}
	for id := envID; id != ""; {
		if _, loop := seen[id]; loop {
			break
		}
		seen[id] = struct{}{}
		l, ok := s.envs[id]
		if !ok {
			break
		}
		out = append(out, l)
		id = l.parentID
	}
	return out
}

func (s *Store) lookupBlob(envID, rel string) ([]byte, bool, bool) {
	for _, l := range s.chain(envID) {
		if b, ok := l.files[rel]; ok {
			return b.data, b.tombstone, true
		}
		if layerMasks(l, rel) {
			return nil, true, true
		}
	}
	return nil, false, false
}

func layerMasks(l *layer, rel string) bool {
	return filesMask(l.files, rel)
}

func filesMask(files map[string]blob, rel string) bool {
	for _, prefix := range ancestorPrefixes(rel) {
		if _, ok := files[prefix]; ok {
			return true
		}
	}
	return false
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
	chain := s.chain(envID)
	for i := len(chain) - 1; i >= 0; i-- {
		for filePath, b := range chain[i].files {
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
