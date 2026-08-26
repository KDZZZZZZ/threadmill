package cmdcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	cache, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func observation(reads map[string]ReadKind, writes ...string) Observation {
	obs := Observation{Reads: reads, Writes: map[string]struct{}{}}
	if obs.Reads == nil {
		obs.Reads = map[string]ReadKind{}
	}
	for _, rel := range writes {
		obs.Writes[rel] = struct{}{}
	}
	return obs
}

var testKey = Key{Command: "go build -o app ./cmd/app", Backend: "bwrap", EnvHash: "env"}

// 两个 agent 的依赖文件版本一致就该复用，哪怕它们的工作区不是同一个目录。
func TestCacheHitAcrossEnvironments(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	if _, err := cache.Store(recorded, testKey, obs, Result{Output: "ok", Duration: time.Second}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("identical dependencies should hit")
	}
	if entry.Output != "ok" {
		t.Fatalf("output = %q, want %q", entry.Output, "ok")
	}
}

// 这条是整个特性存在的理由：无关文件的改动不能让缓存失效。
// 整树指纹在这里必然 miss，读集不会。
func TestCacheHitWhenUnrelatedFileChanges(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
		writeFile(t, root, "README.md", "docs", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)
	writeFile(t, target, "README.md", "another agent edited this", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("an unrelated edit must not invalidate the cache")
	}
}

func TestCacheMissWhenDependencyChanges(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	writeFile(t, recorded, "main.go", "package main", 0o644)
	writeFile(t, recorded, "app", "binary", 0o755)
	writeFile(t, target, "main.go", "package main // changed", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("a changed dependency must miss")
	}
}

// 负依赖：记录时不存在的路径，在别的环境里出现了就必须 miss。
func TestCacheMissWhenAbsentDependencyAppears(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	writeFile(t, recorded, "main.go", "package main", 0o644)
	writeFile(t, target, "main.go", "package main", 0o644)
	writeFile(t, target, "testdata/golden.txt", "new", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile, "testdata": ReadAbsent})
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("an absent dependency that now exists must miss")
	}
}

// 目录依赖：条目集变了就必须 miss，即使每个文件的内容都没动。
func TestCacheMissWhenDirectoryEntryAppears(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "pkg/a.go", "package pkg", 0o644)
	}
	writeFile(t, target, "pkg/b.go", "package pkg", 0o644)

	obs := observation(map[string]ReadKind{"pkg": ReadDir})
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("a new entry in a depended-on directory must miss")
	}
}

// 宿主工具链升级要能让缓存失效。
func TestCacheMissWhenExternalBinaryChanges(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	toolchain := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(toolchain, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, recorded, "main.go", "package main", 0o644)
	writeFile(t, target, "main.go", "package main", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile})
	obs.Externals = []string{toolchain}
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	if entry, err := cache.Lookup(target, testKey); err != nil {
		t.Fatal(err)
	} else if entry == nil {
		t.Fatal("unchanged toolchain should hit")
	}
	if err := os.WriteFile(toolchain, []byte("v2-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("an upgraded toolchain must miss")
	}
}

// 复用必须包含产物，否则命中方拿到的只是一段文字。
func TestReplayRestoresArtifacts(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "build/app", "ELF-ish", 0o755)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "build", "build/app")
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected a hit")
	}
	if err := cache.Replay(target, entry); err != nil {
		t.Fatal(err)
	}
	produced := filepath.Join(target, "build", "app")
	data, err := os.ReadFile(produced)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ELF-ish" {
		t.Fatalf("artifact content = %q", data)
	}
	info, err := os.Stat(produced)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("executable bit must survive replay")
	}
}

func TestReplayRestoresDeletion(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, target, "stale.txt", "old", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "stale.txt")
	if _, err := cache.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("expected a hit")
	}
	if err := cache.Replay(target, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("replay should have deleted the file")
	}
}

// 缓存条目来自磁盘，回放会照着它写文件。被篡改的路径不能借回放写到树外。
func TestReplayRejectsEscapingPath(t *testing.T) {
	cache := newCache(t, Config{})
	target := t.TempDir()
	entry := &Entry{Writes: []Change{{Path: "../escaped.txt", Kind: ChangeDelete}}}
	if err := cache.Replay(target, entry); err == nil {
		t.Fatal("replay must reject a path outside the workspace")
	}
}

// 跨进程共享：另一个 Threadmill 进程指向同一目录就能复用。
func TestCacheSharedAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)

	writer := newCache(t, Config{Dir: dir})
	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	if _, err := writer.Store(recorded, testKey, obs, Result{}); err != nil {
		t.Fatal(err)
	}
	reader := newCache(t, Config{Dir: dir})
	entry, err := reader.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("a separate cache instance on the same directory should hit")
	}
}

func TestStoreRejectsUncacheableObservation(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	writeFile(t, live, "main.go", "package main", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile})
	obs.Network = true
	entry, err := cache.Store(live, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("network traffic must block storing")
	}
	if cache.Stats().Rejected != 1 {
		t.Fatalf("rejected = %d, want 1", cache.Stats().Rejected)
	}
}

func TestStoreHonoursCacheFailuresSetting(t *testing.T) {
	live := t.TempDir()
	writeFile(t, live, "main.go", "package main", 0o644)
	obs := observation(map[string]ReadKind{"main.go": ReadFile})

	off := newCache(t, Config{})
	if entry, err := off.Store(live, testKey, obs, Result{ExitCode: 1}); err != nil {
		t.Fatal(err)
	} else if entry != nil {
		t.Fatal("a failure must not be cached when the setting is off")
	}

	on := newCache(t, Config{CacheFailures: true})
	entry, err := on.Store(live, testKey, obs, Result{ExitCode: 1, Output: "FAIL"})
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("a failure should be cached when the setting is on")
	}
	if entry.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", entry.ExitCode)
	}
}

// 读集过大时校验成本会盖过收益，直接放弃缓存。
func TestStoreRejectsOversizedReadSet(t *testing.T) {
	cache := newCache(t, Config{MaxReadSet: 1})
	live := t.TempDir()
	writeFile(t, live, "a.go", "package a", 0o644)
	writeFile(t, live, "b.go", "package a", 0o644)

	obs := observation(map[string]ReadKind{"a.go": ReadFile, "b.go": ReadFile})
	entry, err := cache.Store(live, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatal("an oversized read set must not be cached")
	}
}

// 产物被容量回收后，对应条目必须当作 miss 并被清掉。
func TestLookupTreatsReclaimedArtifactAsMiss(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	entry, err := cache.Store(recorded, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range entry.Writes {
		if change.Kind == ChangeFile {
			if err := os.Remove(cache.blobs.path(change.Digest)); err != nil {
				t.Fatal(err)
			}
		}
	}
	found, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatal("an entry whose artifacts were reclaimed must miss")
	}
}

func TestInvalidateRemovesEntry(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	writeFile(t, live, "main.go", "package main", 0o644)

	obs := observation(map[string]ReadKind{"main.go": ReadFile})
	entry, err := cache.Store(live, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Invalidate(testKey, entry); err != nil {
		t.Fatal(err)
	}
	found, err := cache.Lookup(live, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatal("an invalidated entry must not come back")
	}
}

func TestGCReclaimsOldestBlobs(t *testing.T) {
	cache := newCache(t, Config{MaxBytes: 64})
	live := t.TempDir()
	writeFile(t, live, "main.go", "package main", 0o644)

	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, live, name, name+"-padding-padding-padding-padding", 0o644)
		obs := observation(map[string]ReadKind{"main.go": ReadFile}, name)
		key := Key{Command: name, Backend: "bwrap"}
		if _, err := cache.Store(live, key, obs, Result{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cache.GC(); err != nil {
		t.Fatal(err)
	}
	_, total, err := cache.blobs.collect()
	if err != nil {
		t.Fatal(err)
	}
	if total > 64 {
		t.Fatalf("blob store = %d bytes, want <= 64", total)
	}
}

// 同一命令在同样的依赖状态下重复执行只应存一条，缓存不能随重跑次数膨胀。
func TestStoreIsIdempotentForSameDependencies(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	writeFile(t, live, "main.go", "package main", 0o644)
	obs := observation(map[string]ReadKind{"main.go": ReadFile})

	first, err := cache.Store(live, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Store(live, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("entry id changed: %q vs %q", first.ID, second.ID)
	}
	entries, err := os.ReadDir(cache.indexDir(testKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("index holds %d entries, want 1", len(entries))
	}
}

// 负依赖的执行前状态由追踪精确给出，不能改用执行后的 lstat：
// 命令可能正是为了创建它才先探测的，执行后它已经存在了。
func TestStoreRecordsProbedAbsenceEvenAfterCreation(t *testing.T) {
	cache := newCache(t, Config{})
	recorded, target := t.TempDir(), t.TempDir()
	for _, root := range []string{recorded, target} {
		writeFile(t, root, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)

	obs := observation(map[string]ReadKind{"main.go": ReadFile, "app": ReadAbsent}, "app")
	entry, err := cache.Store(recorded, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("probe-then-create must be cacheable")
	}
	if entry.Reads["app"] != stateAbsent {
		t.Fatalf("app dependency = %q, want %q", entry.Reads["app"], stateAbsent)
	}
	// 目标环境里 app 也不存在，必须命中并把产物带过去。
	found, err := cache.Lookup(target, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("a fresh environment should hit")
	}
}
