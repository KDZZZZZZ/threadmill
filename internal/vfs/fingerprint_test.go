package vfs

import (
	"os"
	"path/filepath"
	"testing"
)

func fingerprintFixture(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := NewPersistentStore(base, filepath.Join(root, "live"))
	if err != nil {
		t.Fatal(err)
	}
	return store, base
}

func TestStateFingerprintChangesOnOverlayWrite(t *testing.T) {
	store, _ := fingerprintFixture(t)
	view := store.View("env")
	before := store.StateFingerprint("env")
	if err := view.Write("a.txt", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	after := store.StateFingerprint("env")
	if before == after {
		t.Fatalf("fingerprint unchanged after overlay write: %s", after)
	}
}

func TestStateFingerprintChangesOnLiveMutation(t *testing.T) {
	store, _ := fingerprintFixture(t)
	view := store.View("env")
	// 首次写入触发物化，live 树落地。
	if err := view.Write("a.txt", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	before := store.LiveStatHash("env")
	live := liveDirOf(t, store, "env")
	if err := os.WriteFile(filepath.Join(live, "b.txt"), []byte("side effect"), 0o640); err != nil {
		t.Fatal(err)
	}
	after := store.LiveStatHash("env")
	if before == after {
		t.Fatalf("live stat hash unchanged after mutation: %s", after)
	}
}

func TestLiveStatHashChangesOnSameSizeWriteWithRestoredMtime(t *testing.T) {
	store, base := fingerprintFixture(t)
	live := liveDirOf(t, store, "env")
	baseInfo, err := os.Stat(filepath.Join(base, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	before := store.LiveStatHash("env")
	liveFile := filepath.Join(live, "a.txt")
	if err := os.WriteFile(liveFile, []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(liveFile, baseInfo.ModTime(), baseInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after := store.LiveStatHash("env"); before == after {
		t.Fatalf("live stat hash unchanged after same-size write: %s", after)
	}
}

func TestStateFingerprintStableAcrossCalls(t *testing.T) {
	store, _ := fingerprintFixture(t)
	first := store.StateFingerprint("env")
	second := store.StateFingerprint("env")
	if first != second {
		t.Fatalf("fingerprint unstable: %s vs %s", first, second)
	}
}

func TestStateFingerprintSeparatesStores(t *testing.T) {
	storeA, _ := fingerprintFixture(t)
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "a.txt"), []byte("different content same name"), 0o640); err != nil {
		t.Fatal(err)
	}
	storeB, err := NewPersistentStore(base, filepath.Join(t.TempDir(), "live"))
	if err != nil {
		t.Fatal(err)
	}
	if storeA.StateFingerprint("env") == storeB.StateFingerprint("env") {
		t.Fatalf("two stores with different bases must not share fingerprint")
	}
}

func liveDirOf(t *testing.T, store *Store, envID string) string {
	t.Helper()
	live, err := store.Materialize(envID)
	if err != nil {
		t.Fatal(err)
	}
	return live
}
