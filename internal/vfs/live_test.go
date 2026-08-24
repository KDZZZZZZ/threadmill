package vfs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
