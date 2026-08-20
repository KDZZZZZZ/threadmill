package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	defer store.mu.Unlock()
	l, ok := store.envs["env-a"]
	if !ok {
		return
	}
	if _, ok := l.files["hello.txt"]; ok {
		t.Fatal("Absorb copied an unchanged host file into overlay")
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

func TestMergeUpdatesMaterializedLive(t *testing.T) {
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
	if err := store.Merge("child", "parent"); err != nil {
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

func TestMergeAbsorbsChildLiveWrites(t *testing.T) {
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
	if err := store.Merge("child", "parent"); err != nil {
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
