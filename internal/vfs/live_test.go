package vfs

import (
	"os"
	"path/filepath"
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
	store.Fork("", "parent")
	store.Fork("parent", "child")
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
	store.Fork("", "parent")
	store.Fork("parent", "child")
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
