package cmdcache

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestCacheValidatesVirtualMetadataWithoutAllowingContentReads(t *testing.T) {
	live := t.TempDir()
	typ := "dir"
	key := Key{
		Command: "true", Backend: "bwrap", EnvHash: "fixed-layout",
		ExternalPath: func(string) string { return "" },
		ExternalType: func(string) string { return typ },
	}
	cache := newCache(t, Config{})
	obs := observation(nil)
	obs.Externals = []string{"/etc"}
	obs.ExternalReads = map[string]ReadKind{"/etc": ReadStat}
	entry, err := cache.Store(live, key, obs, Result{})
	if err != nil || entry == nil {
		t.Fatalf("virtual directory metadata was not stored: %v, %v", entry, err)
	}
	if hit, err := cache.Lookup(live, key); err != nil || hit == nil {
		t.Fatalf("unchanged virtual node must hit: %v, %v", hit, err)
	}
	typ = "other"
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("changed virtual type must miss: %v, %v", hit, err)
	}
	for _, kind := range []ReadKind{ReadFile, ReadDir} {
		obs.ExternalReads["/etc"] = kind
		entry, err := cache.Store(live, key, obs, Result{})
		if err != nil || entry != nil {
			t.Fatalf("unbacked virtual contents must not be stored: kind %v, entry %v, err %v", kind, entry, err)
		}
	}
	if got := cache.Stats().RejectedReasons["unreadable_external"]; got != 2 {
		t.Fatalf("content rejection count = %d, want 2", got)
	}
}

func TestCacheRejectsExternalProcessWorkingDirectoryProbe(t *testing.T) {
	cache := newCache(t, Config{})
	obs := observation(nil)
	obs.Externals = []string{"/proc/self/cwd"}
	obs.ExternalReads = map[string]ReadKind{"/proc/self/cwd": ReadFile}
	entry, err := cache.Store(t.TempDir(), testKey, obs, Result{})
	if err != nil || entry != nil {
		t.Fatalf("cache process cwd cannot validate the executed command cwd: %v, %v", entry, err)
	}
}

func TestCacheDoesNotReplayEntriesWithoutRootDirectoryTracking(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	key := Key{Command: "ls -1", Backend: "bwrap", EnvHash: "env"}
	legacyHash := sha256.Sum256([]byte("tmcmd1\nls -1\nbwrap\nenv"))
	legacyDir := filepath.Join(cache.dir, "index", fmt.Sprintf("%x", legacyHash))
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"command":"ls -1","backend":"bwrap","env_hash":"env","reads":{},"output":"old.txt\n"}`
	if err := os.WriteFile(filepath.Join(legacyDir, "legacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(live, key)
	if err != nil || entry != nil {
		t.Fatalf("unsafe legacy entry reused: %+v, %v", entry, err)
	}
}

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

func TestCacheNegativeProbeBelowFileHitsUntilParentBecomesDirectory(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	writeFile(t, live, "pricing.py", "price = 1", 0o644)
	obs := observation(map[string]ReadKind{"pricing.py/pyvenv.cfg": ReadAbsent})
	entry, err := cache.Store(live, testKey, obs, Result{})
	if err != nil || entry == nil {
		t.Fatalf("store ENOTDIR negative probe: %v, %v", entry, err)
	}
	if hit, err := cache.Lookup(live, testKey); err != nil || hit == nil {
		t.Fatalf("unchanged ENOTDIR probe must hit: %v, %v", hit, err)
	}
	if err := os.Remove(filepath.Join(live, "pricing.py")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, "pricing.py/pyvenv.cfg", "new environment", 0o644)
	if hit, err := cache.Lookup(live, testKey); err != nil || hit != nil {
		t.Fatalf("new probe target must miss: %v, %v", hit, err)
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

func TestCacheMissWhenExternalFileReplacedWithSameSizeAndMtime(t *testing.T) {
	cache := newCache(t, Config{CacheFailures: true})
	live, site := t.TempDir(), t.TempDir()
	writeFile(t, site, "plugin.py", "old", 0o644)
	dependency := filepath.Join(site, "plugin.py")
	info, err := os.Stat(dependency)
	if err != nil {
		t.Fatal(err)
	}
	obs := observation(nil)
	obs.Externals = []string{dependency}
	if _, err := cache.Store(live, testKey, obs, Result{ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, site, "replacement.py", "new", 0o644)
	replacement := filepath.Join(site, "replacement.py")
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dependency); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(live, testKey)
	if err != nil || entry != nil {
		t.Fatalf("installed plugin replayed old failure: %+v, %v", entry, err)
	}
}

func TestCacheValidatesHomeAcrossEnvironments(t *testing.T) {
	cache := newCache(t, Config{})
	live, homeA, homeB := t.TempDir(), t.TempDir(), t.TempDir()
	for _, home := range []string{homeA, homeB} {
		writeFile(t, home, ".config/tool.conf", "same", 0o644)
	}
	obs := observation(map[string]ReadKind{"~/.config/tool.conf": ReadFile, "~/.config/plugin": ReadAbsent})
	if entry, err := cache.Store(live, testKey, obs, Result{Output: "configured"}, homeA); err != nil || entry == nil {
		t.Fatalf("store HOME input: %+v %v", entry, err)
	}
	if entry, err := cache.Lookup(live, testKey, homeB); err != nil || entry == nil {
		t.Fatalf("identical HOME must hit: %+v %v", entry, err)
	}
	writeFile(t, homeB, ".config/tool.conf", "changed", 0o644)
	if entry, err := cache.Lookup(live, testKey, homeB); err != nil || entry != nil {
		t.Fatalf("changed HOME must miss: %+v %v", entry, err)
	}
	writeFile(t, homeB, ".config/tool.conf", "same", 0o644)
	writeFile(t, homeB, ".config/plugin", "installed", 0o644)
	if entry, err := cache.Lookup(live, testKey, homeB); err != nil || entry != nil {
		t.Fatalf("new HOME plugin must miss: %+v %v", entry, err)
	}
	if entry, err := cache.Lookup(live, testKey); err != nil || entry != nil {
		t.Fatalf("HOME omitted must miss: %+v %v", entry, err)
	}
}

func TestCacheReportsMissKindsRolesAndWeightedHits(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	key := Key{Command: "cat input", Backend: "external", Role: "executor"}
	if _, err := cache.Lookup(live, key); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, "input", "one", 0o644)
	if _, err := cache.Store(live, key, observation(map[string]ReadKind{"input": ReadFile}), Result{Duration: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	key.Role = "verifier"
	hit, err := cache.Lookup(live, key)
	if err != nil || hit == nil || hit.CreatorRole != "executor" {
		t.Fatalf("cross-role hit = %+v %v", hit, err)
	}
	if err := cache.Replay(live, hit, key.Role); err != nil {
		t.Fatal(err)
	}
	writeFile(t, live, "input", "two", 0o644)
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("changed read must miss: %+v %v", hit, err)
	}
	cache.RecordExecution("verifier", 8*time.Second)
	cache.RecordVerification("output")
	stats := cache.Stats()
	if stats.MissNoKey != 1 || stats.MissReadSet != 1 || stats.MissPathTypes["workspace_file"] != 1 {
		t.Fatalf("miss counters = %+v", stats)
	}
	if stats.TimeWeightedHitRate != .2 || stats.SavedDuration != 2*time.Second || stats.Roles["verifier"].Hits != 1 || stats.CrossRoleHits != 1 {
		t.Fatalf("role/weighted counters = %+v", stats)
	}
	if stats.LookupDuration <= 0 || stats.StoreDuration <= 0 || stats.VerifyOutputMismatches != 1 || stats.Verifications != 1 {
		t.Fatalf("timing/audit counters = %+v", stats)
	}
	stats.MissPathTypes["workspace_file"] = 100
	delete(stats.Roles, "verifier")
	if again := cache.Stats(); again.MissPathTypes["workspace_file"] != 1 || again.Roles["verifier"].Hits != 1 {
		t.Fatal("snapshot maps must not mutate live counters")
	}
}

func TestCachePeekDoesNotChangeStatisticsOrEntryAge(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	entry, err := cache.Store(live, testKey, observation(nil), Result{})
	if err != nil || entry == nil {
		t.Fatalf("store: %+v %v", entry, err)
	}
	file := filepath.Join(cache.indexDir(testKey), entry.ID+".json")
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Peek(live, testKey)
	if err != nil || hit == nil {
		t.Fatalf("peek: %+v %v", hit, err)
	}
	after, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if stats := cache.Stats(); stats.Lookups != 0 || stats.Hits != 0 || stats.LookupDuration != 0 || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("peek mutated cache: %+v", stats)
	}
}

func TestCacheNearMissesRemainTelemetryForRawFallbackAndPipelines(t *testing.T) {
	cache := newCache(t, Config{})
	live := t.TempDir()
	key := Key{Command: "echo value", Backend: "external"}
	if _, err := cache.Store(live, key, observation(nil), Result{}); err != nil {
		t.Fatal(err)
	}
	key.Command = "echo   value"
	if hit, err := cache.Lookup(live, key); err != nil || hit == nil {
		t.Fatalf("static AST spelling did not reuse: %+v %v", hit, err)
	}
	key.Command = "echo value | head -1"
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("producer-only similarity reused: %+v %v", hit, err)
	}
	key.Command = "eval 'printf value'"
	if entry, err := cache.Store(live, key, observation(nil), Result{}); err != nil || entry == nil {
		t.Fatalf("store raw fallback: %+v %v", entry, err)
	}
	key.Command = "eval  'printf value'"
	if hit, err := cache.Lookup(live, key); err != nil || hit != nil {
		t.Fatalf("raw fallback similarity reused: %+v %v", hit, err)
	}
	if stats := cache.Stats(); stats.NearMissAST != 1 || stats.NearMissPipeline != 1 || stats.Hits != 1 {
		t.Fatalf("near miss counters: %+v", stats)
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
	stats := cache.Stats()
	if stats.ArtifactReflinks+stats.ArtifactCopies != 1 {
		t.Fatalf("artifact replay stats = %+v, want one materialized file", stats)
	}
	if stats.ReflinkBytes+stats.CopiedBytes != uint64(len("ELF-ish")) {
		t.Fatalf("artifact replay bytes = %+v, want %d", stats, len("ELF-ish"))
	}
}

func TestReplayReflinksArtifactsWhenSupported(t *testing.T) {
	root := t.TempDir()
	cache := newCache(t, Config{Dir: filepath.Join(root, "cache")})
	recorded := filepath.Join(root, "recorded")
	target := filepath.Join(root, "target")
	for _, dir := range []string{recorded, target} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "main.go", "package main", 0o644)
	}
	writeFile(t, recorded, "app", "binary", 0o755)

	obs := observation(map[string]ReadKind{"main.go": ReadFile}, "app")
	entry, err := cache.Store(recorded, testKey, obs, Result{})
	if err != nil {
		t.Fatal(err)
	}
	source, err := cache.blobs.open(entry.Writes[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := os.CreateTemp(target, ".reflink-probe-")
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	cloneErr := unix.IoctlFileClone(int(probe.Fd()), int(source.Fd()))
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probe.Name()); err != nil {
		t.Fatal(err)
	}
	if cloneErr != nil {
		t.Skipf("reflink unavailable on test filesystem: %v", cloneErr)
	}

	if err := cache.Replay(target, entry); err != nil {
		t.Fatal(err)
	}
	stats := cache.Stats()
	if stats.ArtifactReflinks != 1 || stats.ArtifactCopies != 0 {
		t.Fatalf("artifact replay stats = %+v, want one reflink and no copies", stats)
	}
	if stats.ReflinkBytes != uint64(len("binary")) || stats.CopiedBytes != 0 {
		t.Fatalf("artifact replay bytes = %+v, want reflink bytes only", stats)
	}
	if err := os.WriteFile(filepath.Join(target, "app"), []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(cache.blobs.path(entry.Writes[0].Digest))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "binary" {
		t.Fatalf("mutating the clone changed its CAS source: %q", stored)
	}
}

func TestStoreRejectsArtifactsWithoutReflink(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "threadmill-cache-test-")
	if err != nil {
		t.Skipf("tmpfs fixture unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	cache := newCache(t, Config{Dir: filepath.Join(root, "cache")})
	work := filepath.Join(root, "work")
	writeFile(t, work, "app", "binary", 0o755)
	if _, err := cache.Store(work, testKey, observation(nil, "app"), Result{}); err == nil {
		t.Fatal("cache copied an artifact on a filesystem without reflink")
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
