package vfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestMaterializeIsIdempotent(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	first, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("Materialize = %q then %q, want the same live dir", first, second)
	}
	got, err := os.ReadFile(filepath.Join(first, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("live hello.txt = %q, want hello", got)
	}
	stats := store.Stats()
	if stats.MaterializeCopies != 1 || stats.MaterializeCopyErrors != 0 || stats.MaterializeCopyDuration <= 0 {
		t.Fatalf("materialize stats = %+v, want one successful measured copy", stats)
	}
	if got := stats.MaterializeOverlays + stats.MaterializeReflinks + stats.MaterializeFullCopies; got != 1 {
		t.Fatalf("materialize backend total = %d, want one classified materialization: %+v", got, stats)
	}
}

func TestMaterializeCoalescesConcurrentCalls(t *testing.T) {
	store, base := newTestStore(t)
	t.Cleanup(func() { _ = store.Discard("env-a") })
	for i := range 1000 {
		mustWriteFile(t, filepath.Join(base, "fixture", fmt.Sprintf("file-%04d", i)), "x")
	}

	const callers = 16
	start := make(chan struct{})
	dirs := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dirs[i], errs[i] = store.Materialize("env-a")
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Materialize caller %d: %v", i, err)
		}
		if dirs[i] != dirs[0] {
			t.Fatalf("Materialize caller %d dir = %q, want %q", i, dirs[i], dirs[0])
		}
	}
	if got := store.Stats().MaterializeCopies; got != 1 {
		t.Fatalf("materialize copies = %d, want one coalesced copy", got)
	}
}

func TestMaterializeBoundsConcurrentCopies(t *testing.T) {
	const callers = 16
	store, base := newTestStore(t)
	t.Cleanup(func() {
		for i := range callers {
			_ = store.Discard(fmt.Sprintf("env-%d", i))
		}
	})
	for i := range 500 {
		mustWriteFile(t, filepath.Join(base, "fixture", fmt.Sprintf("file-%04d", i)), "x")
	}

	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = store.Materialize(fmt.Sprintf("env-%d", i))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Materialize caller %d: %v", i, err)
		}
	}

	stats := store.Stats()
	if stats.MaterializeCapacity <= 0 {
		t.Fatalf("materialize capacity = %d, want positive bound", stats.MaterializeCapacity)
	}
	if stats.MaterializePeakActive > stats.MaterializeCapacity {
		t.Fatalf("materialize peak = %d, capacity = %d", stats.MaterializePeakActive, stats.MaterializeCapacity)
	}
	if stats.MaterializeCopies != callers {
		t.Fatalf("materialize copies = %d, want %d distinct environments", stats.MaterializeCopies, callers)
	}
}

func TestAbsorbBoundsConcurrentScans(t *testing.T) {
	const callers = 16
	store, base := newTestStore(t)
	for i := range 500 {
		mustWriteFile(t, filepath.Join(base, "fixture", fmt.Sprintf("file-%04d", i)), "x")
	}
	for i := range callers {
		envID := fmt.Sprintf("env-%d", i)
		t.Cleanup(func() { _ = store.Discard(envID) })
		live, err := store.Materialize(envID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(live, "hello.txt"), []byte("world"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = store.Absorb(fmt.Sprintf("env-%d", i))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Absorb caller %d: %v", i, err)
		}
	}

	stats := store.Stats()
	if stats.AbsorbCapacity <= 0 {
		t.Fatalf("absorb capacity = %d, want positive bound", stats.AbsorbCapacity)
	}
	if stats.AbsorbPeakActive > stats.AbsorbCapacity {
		t.Fatalf("absorb peak = %d, capacity = %d", stats.AbsorbPeakActive, stats.AbsorbCapacity)
	}
	if stats.AbsorbScans != callers {
		t.Fatalf("absorb scans = %d, want %d", stats.AbsorbScans, callers)
	}
}

func TestMaterializeChildWriteSurvivesParentDirTombstone(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "parent")
	mustFork(t, store, "parent", "child")
	if err := store.View("parent").Delete("sub"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Write("sub/nested.txt", []byte("revived")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "sub", "nested.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "revived" {
		t.Fatalf("live sub/nested.txt = %q, want revived", got)
	}
}

func TestMaterializeAppliesOverlayWrites(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("env-a").Write("overlay.txt", []byte("from-overlay")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "overlay.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-overlay" {
		t.Fatalf("live overlay.txt = %q, want from-overlay", got)
	}
}

func TestMaterializeChildUsesFrozenSnapshot(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("parent").Write("base-mod.txt", []byte("p1")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := store.View("parent").Write("parent-after.txt", []byte("after")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("parent").Write("base-mod.txt", []byte("p2")); err != nil {
		t.Fatal(err)
	}

	live, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "base-mod.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "p1" {
		t.Fatalf("live base-mod.txt = %q, want p1", got)
	}
	if _, err := os.Stat(filepath.Join(live, "parent-after.txt")); !os.IsNotExist(err) {
		t.Fatalf("child live materialized parent's post-fork file: %v", err)
	}
}

func TestReleaseRemovesLiveDir(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("live dir still exists after Release: %v", err)
	}
}

func TestLiveRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "leak")
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize("env-a"); err != nil {
		t.Fatal(err)
	}
	view := store.View("env-a")
	if _, err := view.Read("link/secret.txt"); err == nil {
		t.Fatal("Read followed a symlink out of live")
	}
	if _, err := view.Stat("link/secret.txt"); err == nil {
		t.Fatal("Stat followed a symlink out of live")
	}
	if _, err := view.List("link"); err == nil {
		t.Fatal("List followed a symlink out of live")
	}
	if err := view.Write("link/pwn.txt", []byte("x")); err == nil {
		t.Fatal("Write followed a symlink out of live")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn.txt")); err == nil {
		t.Fatal("wrote outside the live dir via symlink")
	}
}

func TestFileViewReadSeesLiveWrite(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "from-bash.txt"), []byte("from-live"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("env-a").Read("from-bash.txt")
	if err != nil {
		t.Fatalf("FileView missed live write: %v", err)
	}
	if string(got) != "from-live" {
		t.Fatalf("from-bash.txt = %q, want from-live", got)
	}
}

func TestFileViewWriteAfterMaterializeUpdatesLive(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View("env-a").Write("via-view.txt", []byte("from-view")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "via-view.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-view" {
		t.Fatalf("live via-view.txt = %q, want from-view", got)
	}
}

func TestMaterializeReplacesHostDirWithOverlayFile(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("env-a").Write("sub", []byte("now-a-file")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "now-a-file" {
		t.Fatalf("live sub = %q, want now-a-file", got)
	}
}

func TestAbsorbPicksUpLiveWrites(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "from-bash.txt"), []byte("from-live"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("env-a").Read("from-bash.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "from-live" {
		t.Fatalf("overlay from-bash.txt = %q, want from-live", got)
	}
}

func TestAbsorbPicksUpSameSizeWriteWithRestoredMtime(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	baseInfo, err := os.Stat(filepath.Join(base, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	liveFile := filepath.Join(live, "hello.txt")
	if err := os.WriteFile(liveFile, []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(liveFile, baseInfo.ModTime(), baseInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "parent", "child")
	got, err := store.View("child").Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("child hello.txt = %q, want same-size live write", got)
	}
	stats := store.Stats()
	if stats.AbsorbScans != 1 || stats.AbsorbScanErrors != 0 {
		t.Fatalf("absorb scan stats = %+v, want one successful content scan", stats)
	}
}

func TestAbsorbComparesOnlyChangedStatBuckets(t *testing.T) {
	store, base := newTestStore(t)
	for i := range 512 {
		mustWriteFile(t, filepath.Join(base, "fixture", fmt.Sprintf("file-%04d.txt", i)), "fixture")
	}
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(live, "fixture", "file-0000.txt"),
		[]byte("changed"),
		0o640,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("env-a"); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().AbsorbContentComparisons; got >= 64 {
		t.Fatalf("content comparisons = %d, want fewer than one stat bucket", got)
	}
	got, err := store.View("env-a").Read("fixture/file-0000.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "changed" {
		t.Fatalf("changed file = %q, want changed", got)
	}
}

func TestAbsorbTombsLiveDeletions(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "hello.txt")); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("env-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.View("env-a").Read("hello.txt"); err == nil {
		t.Fatal("Absorb left a live deletion visible in overlay")
	}
}

func TestAbsorbKeepsDeletedDirectoriesAbsentAfterFork(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	seedDirectoryDeletionFixture(t, base)
	store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: false})
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "", "parent")
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	removeDirectoryDeletionFixture(t, live)

	mustFork(t, store, "parent", "child")
	assertDirectoryTombstones(t, store, "parent")
	childLive, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryDeletionFixtureAbsent(t, childLive)
	stats := store.Stats()
	if stats.MaterializeOverlays != 0 ||
		stats.MaterializeReflinks+stats.MaterializeFullCopies == 0 {
		t.Fatalf("materialize stats = %+v, want copy/reflink workspace", stats)
	}
}

func TestAbsorbKeepsDeletedDirectoriesAbsentAfterRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	seedDirectoryDeletionFixture(t, base)
	first, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: false})
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, first, "", "parent")
	live, err := first.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	removeDirectoryDeletionFixture(t, live)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Restore("parent"); err != nil {
		t.Fatal(err)
	}
	mustFork(t, restarted, "parent", "child")
	assertDirectoryTombstones(t, restarted, "parent")
	childLive, err := restarted.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	assertDirectoryDeletionFixtureAbsent(t, childLive)
}

func TestAbsorbSkipsUnchangedHostFiles(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if _, err := store.Materialize("env-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("env-a"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	l, ok := store.envs["env-a"]
	if !ok {
		store.mu.Unlock()
		return
	}
	if _, ok := l.files["hello.txt"]; ok {
		store.mu.Unlock()
		t.Fatal("Absorb copied an unchanged host file into overlay")
	}
	store.mu.Unlock()
	if stats := store.Stats(); stats.AbsorbFastPaths != 1 || stats.AbsorbScans != 0 {
		t.Fatalf("absorb fast-path stats = %+v, want one metadata-only absorb", stats)
	}
}

func TestMaterializeAppliesOverlayTombstone(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("env-a").Delete("hello.txt"); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(live, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("live still has tombstoned hello.txt: %v", err)
	}
}

func TestApplyJoinSafeUpdatesMaterializedLive(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "parent")
	mustFork(t, store, "parent", "child")
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Write("from-child.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := applySafeJoin(store, "child", "parent"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(live, "from-child.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Fatalf("live from-child.txt = %q, want hi", got)
	}
}

func TestForkAbsorbsParentLiveIntoChildSnapshot(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "parent")
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "from-live.txt"), []byte("at-fork"), 0o640); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := os.WriteFile(filepath.Join(live, "from-live.txt"), []byte("after-fork"), 0o640); err != nil {
		t.Fatal(err)
	}

	got, err := store.View("child").Read("from-live.txt")
	if err != nil {
		t.Fatalf("child missed parent live write: %v", err)
	}
	if string(got) != "at-fork" {
		t.Fatalf("child from-live.txt = %q, want at-fork", got)
	}
}

func TestForkPreservesExecutableBitFromParentLive(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	script := filepath.Join(base, "script.sh")
	mustWriteFile(t, script, "#!/bin/sh\nexit 0\n")
	mustFork(t, store, "", "parent")
	parentLive, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	liveScript := filepath.Join(parentLive, "script.sh")
	if err := os.WriteFile(liveScript, []byte("#!/bin/sh\nexit 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(liveScript, 0o755); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "parent", "child")
	childLive, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(childLive, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("child script mode = %o, want executable", info.Mode().Perm())
	}
}

func TestWritePreservesExistingExecutableBit(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	script := filepath.Join(base, "script.sh")
	mustWriteFile(t, script, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View("env-a").Write("script.sh", []byte("#!/bin/sh\nexit 1\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(live, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("edited script mode = %o, want executable", info.Mode().Perm())
	}
}

func TestForkPreservesChmodOnly(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	script := filepath.Join(base, "script.sh")
	mustWriteFile(t, script, "#!/bin/sh\nexit 0\n")
	mustFork(t, store, "", "parent")
	parentLive, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(parentLive, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "parent", "child")
	childLive, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(childLive, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("child script mode = %o, want executable", info.Mode().Perm())
	}
}

func TestApplyJoinSafePreservesExecutableBit(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	script := filepath.Join(base, "script.sh")
	mustWriteFile(t, script, "#!/bin/sh\nexit 0\n")
	mustFork(t, store, "", "parent")
	mustFork(t, store, "parent", "child")
	childLive, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(childLive, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := applySafeJoin(store, "child", "parent"); err != nil {
		t.Fatal(err)
	}
	parentLive, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(parentLive, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("merged script mode = %o, want executable", info.Mode().Perm())
	}
}

func TestApplyJoinSafeAbsorbsChildLiveWrites(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "parent")
	mustFork(t, store, "parent", "child")
	live, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "from-live.txt"), []byte("from-live"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := applySafeJoin(store, "child", "parent"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("parent").Read("from-live.txt")
	if err != nil {
		t.Fatalf("merge missed child live write: %v", err)
	}
	if string(got) != "from-live" {
		t.Fatalf("parent from-live.txt = %q, want from-live", got)
	}
}

func TestReleaseAbsorbsLiveWrites(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "from-live.txt"), []byte("from-live"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("env-a").Read("from-live.txt")
	if err != nil {
		t.Fatalf("release dropped live write: %v", err)
	}
	if string(got) != "from-live" {
		t.Fatalf("overlay from-live.txt = %q, want from-live", got)
	}
}

func TestPublishCommitsEnvironmentToBase(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	mustFork(t, store, "", "root")
	if err := store.View("root").Write("hello.txt", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("root").Write("new/script.sh", []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("root").Delete("sub"); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("base changed before publish: %q, %v", got, err)
	}
	receipt, err := store.Publish("root")
	if err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "changed" {
		t.Fatalf("published hello.txt = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "new", "script.sh")); err != nil || string(got) != "#!/bin/sh\n" {
		t.Fatalf("published new/script.sh = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "sub")); !os.IsNotExist(err) {
		t.Fatalf("published deletion left sub: %v", err)
	}
	if receipt.Changed() == 0 {
		t.Fatalf("receipt reported no change: %+v", receipt)
	}
	if !slices.Contains(receipt.Updated, "hello.txt") ||
		!slices.Contains(receipt.Added, "new/script.sh") {
		t.Fatalf("receipt missing rendered paths: %+v", receipt)
	}
	// Publication is a display operation: the checkpoint it rendered from is
	// still there to render again.
	if stats := store.Stats(); stats.Environments == 0 {
		t.Fatalf("publication consumed the checkpoint: %+v", stats)
	}
}

func TestPublishCommitsLiveDirectoryDeletions(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	seedDirectoryDeletionFixture(t, base)
	store := NewStore(base)
	mustFork(t, store, "", "root")
	live, err := store.Materialize("root")
	if err != nil {
		t.Fatal(err)
	}
	removeDirectoryDeletionFixture(t, live)

	if _, err := store.Publish("root"); err != nil {
		t.Fatal(err)
	}
	assertDirectoryDeletionFixtureAbsent(t, base)
}

func TestPublishCountsNoopAsCommit(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "root")
	receipt, err := store.Publish("root")
	if err != nil {
		t.Fatal(err)
	}
	// A checkpoint that matches what is displayed is a legitimate publication
	// that changed nothing, and the receipt has to say so.
	if receipt.Changed() != 0 {
		t.Fatalf("no-op publish receipt = %+v, want no change", receipt)
	}
	stats := store.Stats()
	if stats.PublishAttempts != 1 || stats.PublishCommits != 1 || stats.PublishErrors != 0 {
		t.Fatalf("no-op publish stats = %+v", stats)
	}
}

func TestFreezeRetainsSnapshotWithoutLiveWorkspace(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	mustFork(t, store, "", "root")
	live, err := store.Materialize("root")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "frozen.txt"), []byte("kept"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze("root"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "frozen.txt")); !os.IsNotExist(err) {
		t.Fatalf("Freeze changed base: %v", err)
	}
	got, err := store.View("root").Read("frozen.txt")
	if err != nil || string(got) != "kept" {
		t.Fatalf("frozen snapshot = %q, %v", got, err)
	}
	if stats := store.Stats(); stats.Environments != 1 || stats.LiveDirs != 0 {
		t.Fatalf("Freeze stats = %+v, want one environment and no live dirs", stats)
	}
}

func TestArchiveRestoresAndPublishesAfterRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	store, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "", "source")
	live, err := store.Materialize("source")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(live, "nested", "archived.txt"), "kept")
	if err := store.Archive("source", "archive"); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("source"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Restore("archive"); err != nil {
		t.Fatalf("restore archive: %v", err)
	}
	if _, err := restarted.Publish("archive"); err != nil {
		t.Fatalf("publish restored archive: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(base, "nested", "archived.txt"))
	if err != nil || string(got) != "kept" {
		t.Fatalf("published archive = %q, %v, want kept", got, err)
	}
}

func TestArchiveRejectsUnknownSource(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	err := store.Archive("missing", "archive")
	if !errors.Is(err, ErrUnknownEnvironment) {
		t.Fatalf("Archive() error = %v, want ErrUnknownEnvironment", err)
	}
	if stats := store.Stats(); stats.Environments != 0 || stats.LiveDirs != 0 {
		t.Fatalf("failed archive created state: %+v", stats)
	}
}

func TestPublishDoesNotCommitGitMetadata(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	mustWriteFile(t, filepath.Join(base, ".git", "index"), "host-index")
	mustFork(t, store, "", "root")
	live, err := store.Materialize("root")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, ".git", "index"), []byte("agent-index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "hello.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Publish("root"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, ".git", "index")); err != nil || string(got) != "host-index" {
		t.Fatalf("published .git/index = %q, %v, want host metadata unchanged", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "changed" {
		t.Fatalf("published hello.txt = %q, %v, want changed", got, err)
	}
}

func TestPublishProceedsWithActiveSibling(t *testing.T) {
	t.Parallel()

	// Publication used to wait for every sibling to stop, because it wrote into
	// the overlay lower directory. It renders onto the display surface instead,
	// so a running sibling is no longer its business.
	store, base := newTestStore(t)
	mustFork(t, store, "", "root")
	if err := store.View("root").Write("hello.txt", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	store.mountMu.Lock()
	store.mounts["sibling"] = &overlayMount{}
	store.mountMu.Unlock()
	t.Cleanup(func() {
		store.mountMu.Lock()
		delete(store.mounts, "sibling")
		store.mountMu.Unlock()
	})

	if _, err := store.Publish("root"); err != nil {
		t.Fatalf("Publish() with active sibling: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "changed" {
		t.Fatalf("published hello.txt = %q, %v, want changed", got, err)
	}
}

func TestPublishLeavesDisplayOnlyFiles(t *testing.T) {
	t.Parallel()

	// The display surface is the user's own directory. A build artifact they or
	// their tooling created after the session adopted the project was never in
	// any checkpoint, so no checkpoint may remove it; a file that did come from
	// the project still goes when the checkpoint drops it.
	store, base := newTestStore(t)
	mustFork(t, store, "", "root")
	if err := store.View("root").Delete("sub/nested.txt"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(base, "build", "artifact.o"), "local")

	if _, err := store.Publish("root"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "build", "artifact.o")); err != nil || string(got) != "local" {
		t.Fatalf("publication removed a display-only file: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "sub", "nested.txt")); !os.IsNotExist(err) {
		t.Fatalf("publication kept a dropped project file: %v", err)
	}
}

func TestPublishRetainsDisplacedDisplayContent(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "hello")
	store, err := NewPersistentStore(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "", "root")
	if err := store.View("root").Write("hello.txt", []byte("from-checkpoint")); err != nil {
		t.Fatal(err)
	}
	// Someone edited the project directly before the checkpoint was rendered.
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "hand-edited")

	receipt, err := store.Publish("root")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Replaced == "" {
		t.Fatalf("receipt did not record displaced content: %+v", receipt)
	}
	got, err := os.ReadFile(filepath.Join(receipt.Replaced, "hello.txt"))
	if err != nil || string(got) != "hand-edited" {
		t.Fatalf("retained content = %q, %v, want hand-edited", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "from-checkpoint" {
		t.Fatalf("published hello.txt = %q, %v", got, err)
	}
}

func TestPublishRendersAnEarlierCheckpointAgain(t *testing.T) {
	t.Parallel()

	// Checkpoints are versions to show, not a ratchet: going back is rendering
	// the earlier one again.
	store, base := newTestStore(t)
	mustFork(t, store, "", "first")
	if err := store.View("first").Write("hello.txt", []byte("first")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "first", "second")
	if err := store.View("second").Write("hello.txt", []byte("second")); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Publish("second"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "second" {
		t.Fatalf("hello.txt = %q, %v, want second", got, err)
	}
	receipt, err := store.Publish("first")
	if err != nil {
		t.Fatalf("re-render earlier checkpoint: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "hello.txt")); err != nil || string(got) != "first" {
		t.Fatalf("hello.txt = %q, %v, want first", got, err)
	}
	if !slices.Contains(receipt.Updated, "hello.txt") {
		t.Fatalf("receipt missing the reverted path: %+v", receipt)
	}
}

func TestPersistentPublishLeavesEnvironmentReadsAlone(t *testing.T) {
	t.Parallel()

	// The whole point of the floor: what a running environment reads must not
	// move when a checkpoint is rendered for the user.
	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "original")
	store, err := NewPersistentStore(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "", "done")
	if err := store.View("done").Write("hello.txt", []byte("published")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "", "running")

	if _, err := store.Publish("done"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("running").Read("hello.txt")
	if err != nil || string(got) != "original" {
		t.Fatalf("running environment read %q, %v, want the floor's original", got, err)
	}
}

func TestPersistentFloorRetakenWhenProjectChangedBetweenSessions(t *testing.T) {
	t.Parallel()

	// A later session has to build on what the user can actually see, so a
	// project edited outside Threadmill is re-adopted as the new floor.
	base := t.TempDir()
	state := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "original")
	store, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "edited-between-sessions")

	restarted, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, restarted, "", "next")
	got, err := restarted.View("next").Read("hello.txt")
	if err != nil || string(got) != "edited-between-sessions" {
		t.Fatalf("new session read %q, %v, want the edited project", got, err)
	}
}

func TestStoreDiscardDropsOverlayAndUnabsorbedLiveWrites(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "parent")
	mustFork(t, store, "parent", "scratch")
	if err := store.View("scratch").Write("overlay.txt", []byte("overlay")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "live.txt"), []byte("live"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := store.Discard("scratch"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); !os.IsNotExist(err) {
		t.Fatalf("discarded live dir still exists: %v", err)
	}
	for _, path := range []string{"overlay.txt", "live.txt"} {
		if _, err := store.View("scratch").Read(path); err == nil {
			t.Fatalf("%s survived discard: %v", path, err)
		}
	}

	mustFork(t, store, "parent", "scratch")
	if _, err := store.View("scratch").Read("overlay.txt"); err == nil {
		t.Fatalf("reused environment kept stale overlay: %v", err)
	}
}

func TestStoreDiscardKeepsTrackingWhenRemovalFails(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "", "scratch")
	live, err := store.Materialize("scratch")
	if err != nil {
		t.Fatal(err)
	}
	invalidLive := string([]byte{0})
	store.mu.Lock()
	store.lives["scratch"] = invalidLive
	store.mu.Unlock()

	if err := store.Discard("scratch"); err == nil {
		t.Fatal("Discard() succeeded for an invalid live path")
	}
	store.mu.Lock()
	tracked := store.lives["scratch"] == invalidLive && store.envs["scratch"] != nil
	store.lives["scratch"] = live
	store.mu.Unlock()
	if !tracked {
		t.Fatal("Discard() forgot a workspace that still needs cleanup")
	}

	if err := store.Discard("scratch"); err != nil {
		t.Fatalf("retry Discard() error = %v", err)
	}
}

func TestPersistentStoreRestoresReleasedEnvironment(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "host")
	state := t.TempDir()

	first, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, first, "", "env-a")
	if err := first.View("env-a").Write("hello.txt", []byte("task")); err != nil {
		t.Fatal(err)
	}
	if err := first.Release("env-a"); err != nil {
		t.Fatal(err)
	}

	second, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, second, "", "env-a")
	got, err := second.View("env-a").Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "task" {
		t.Fatalf("restored hello.txt = %q, want task", got)
	}

	if err := second.Discard("env-a"); err != nil {
		t.Fatal(err)
	}
	third, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, third, "", "env-a")
	got, err = third.View("env-a").Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "host" {
		t.Fatalf("discarded hello.txt = %q, want host", got)
	}
}

func TestPersistentHandoffMovesReleasedLiveAcrossRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "hello.txt"), "host")
	state := t.TempDir()

	first, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, first, "", "parent")
	parentLive, err := first.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parentLive, "hello.txt"), []byte("parent"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Release("parent"); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(parentLive)
	if err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Handoff("parent", "child"); err != nil {
		t.Fatal(err)
	}
	childLive, err := restarted.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	childInfo, err := os.Stat(childLive)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(parentInfo, childInfo) {
		t.Fatal("Handoff copied the persistent live directory")
	}
	got, err := restarted.View("child").Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "parent" {
		t.Fatalf("child hello.txt = %q, want parent", got)
	}
	if _, err := os.Stat(parentLive); !os.IsNotExist(err) {
		t.Fatalf("parent live path still exists after handoff: %v", err)
	}
	if got := restarted.Stats().Handoffs; got != 1 {
		t.Fatalf("handoffs = %d, want 1", got)
	}

	retried, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := retried.Handoff("parent", "child"); err != nil {
		t.Fatalf("retry Handoff() error = %v", err)
	}
	got, err = retried.View("child").Read("hello.txt")
	if err != nil || string(got) != "parent" {
		t.Fatalf("retried child hello.txt = %q, %v", got, err)
	}
}

func TestAbsorbRejectsSpecialFiles(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(live, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo not supported: %v", err)
	}
	err = store.Absorb("env-a")
	if err == nil || !errors.Is(err, ErrSpecialFile) {
		t.Fatalf("Absorb error = %v, want ErrSpecialFile", err)
	}
}

func TestAbsorbRejectsFileExceedingMaxSize(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(live, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileSize + 1)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	err = store.Absorb("env-a")
	if err == nil || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Absorb error = %v, want ErrFileTooLarge", err)
	}
}

func TestAbsorbExcludesGitIgnoredWorkspaceArtifacts(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "source.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "node_modules", "cache.txt"), []byte("base cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "node_modules", "tracked.txt"), []byte("tracked before"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", base)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	cmd = exec.Command("git", "-C", base, "add", "--force", "node_modules/tracked.txt")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add tracked ignored file: %v: %s", err, output)
	}

	store := NewStore(base)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "source.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "node_modules", "cache.txt"), []byte("changed cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "node_modules", "tracked.txt"), []byte("tracked after"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(live, "node_modules", "package", "artifact.bin")
	if err := os.MkdirAll(filepath.Dir(generated), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(generated)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileSize + 1)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.Absorb("env-a"); err != nil {
		t.Fatalf("Absorb with ignored generated dependency: %v", err)
	}
	if err := store.Fork("env-a", "child"); err != nil {
		t.Fatal(err)
	}
	child, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(child, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Fatalf("child source.txt = %q, want after", got)
	}
	got, err = os.ReadFile(filepath.Join(child, "node_modules", "cache.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "base cache" {
		t.Fatalf("child ignored base cache = %q, want unchanged base cache", got)
	}
	got, err = os.ReadFile(filepath.Join(child, "node_modules", "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tracked after" {
		t.Fatalf("child tracked ignored file = %q, want tracked after", got)
	}
	if _, err := os.Stat(filepath.Join(child, "node_modules", "package")); !os.IsNotExist(err) {
		t.Fatalf("new ignored package exists in child: %v", err)
	}
}

func TestAbsorbDoesNotUseHostGlobalGitIgnore(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "source.generated"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", base)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	globalIgnore := filepath.Join(t.TempDir(), "global-ignore")
	if err := os.WriteFile(globalIgnore, []byte("*.generated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	config := "[core]\n\texcludesFile = " + globalIgnore + "\n"
	if err := os.WriteFile(globalConfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	store := NewStore(base)
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "source.generated"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	got, err := store.View("child").Read("source.generated")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Fatalf("child source.generated = %q, want after", got)
	}
}

func TestAbsorbKeepsUnchangedLargeBaseFileOutsideOverlayLimit(t *testing.T) {
	base := t.TempDir()
	pack := filepath.Join(base, ".git", "objects", "pack", "base.pack")
	if err := os.MkdirAll(filepath.Dir(pack), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileSize + 1)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store := NewStore(base)
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("parent"); err != nil {
		t.Fatalf("Absorb unchanged large base file: %v", err)
	}
	if err := os.Remove(filepath.Join(live, ".git", "objects", "pack", "base.pack")); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	child, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(child, ".git", "objects", "pack", "base.pack")); !os.IsNotExist(err) {
		t.Fatalf("deleted large base file exists in child: %v", err)
	}
}

func TestAbsorbRejectsChangedLargeBaseFile(t *testing.T) {
	base := t.TempDir()
	large := filepath.Join(base, "base.bin")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileSize + 1)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store := NewStore(base)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	f, err = os.OpenFile(filepath.Join(live, "base.bin"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1}); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = store.Absorb("env-a")
	if err == nil || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Absorb error = %v, want ErrFileTooLarge", err)
	}
}

func TestAbsorbRejectsTotalSizeExceeded(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	// 创建多个文件，单个不超限，但累加超总上限
	const perFileSize = 40 * 1024 * 1024
	numFiles := int((MaxTotalSize / perFileSize) + 1)
	for i := 0; i < numFiles; i++ {
		p := filepath.Join(live, fmt.Sprintf("chunk%d.bin", i))
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(perFileSize); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		_ = f.Close()
	}

	err = store.Absorb("env-a")
	if err == nil || !errors.Is(err, ErrTotalSizeExceeded) {
		t.Fatalf("Absorb error = %v, want ErrTotalSizeExceeded", err)
	}
}

func seedDirectoryDeletionFixture(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{
		"m4/macros/compiler.m4",
		"m4/nested/platform.m4",
		"build-aux/install-sh",
		"build-aux/nested/missing",
	} {
		mustWriteFile(t, filepath.Join(root, filepath.FromSlash(path)), path)
	}
}

func removeDirectoryDeletionFixture(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"m4", "build-aux"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			t.Fatalf("remove %s: %v", name, err)
		}
	}
}

func assertDirectoryDeletionFixtureAbsent(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"m4", "build-aux"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived directory deletion: %v", name, err)
		}
	}
}

func assertDirectoryTombstones(t *testing.T, store *Store, envID string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	layer := store.envs[envID]
	if layer == nil {
		t.Fatalf("environment %q is missing", envID)
	}
	for _, name := range []string{"m4", "build-aux"} {
		item, ok := layer.files[name]
		if !ok || !item.tombstone {
			t.Fatalf("overlay %q = %+v, %v, want directory tombstone", name, item, ok)
		}
		prefix := name + "/"
		for path := range layer.files {
			if strings.HasPrefix(path, prefix) {
				t.Fatalf("overlay contains redundant descendant %q below %q", path, name)
			}
		}
	}
}

func TestFileViewReadRejectsSpecialFileInLive(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(live, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo not supported: %v", err)
	}
	_, err = store.View("env-a").Read("pipe")
	if err == nil || !errors.Is(err, ErrSpecialFile) {
		t.Fatalf("Read special file error = %v, want ErrSpecialFile", err)
	}
}

func TestFileViewReadRejectsTooLargeFileInLive(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	live, err := store.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(live, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxFileSize + 1)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	_, err = store.View("env-a").Read("big.bin")
	if err == nil || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Read large file error = %v, want ErrFileTooLarge", err)
	}
}
