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

func TestScanLiveFingerprintChangesOnLiveMutation(t *testing.T) {
	store, _ := fingerprintFixture(t)
	view := store.View("env")
	if err := view.Write("a.txt", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	live := liveDirOf(t, store, "env")
	before := scanLiveFingerprint(live).hash
	if err := os.WriteFile(filepath.Join(live, "b.txt"), []byte("side effect"), 0o640); err != nil {
		t.Fatal(err)
	}
	after := scanLiveFingerprint(live).hash
	if before == after {
		t.Fatalf("live stat hash unchanged after mutation: %s", after)
	}
}

func TestScanLiveFingerprintChangesOnSameSizeWriteWithRestoredMtime(t *testing.T) {
	store, base := fingerprintFixture(t)
	live := liveDirOf(t, store, "env")
	baseInfo, err := os.Stat(filepath.Join(base, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	before := scanLiveFingerprint(live).hash
	liveFile := filepath.Join(live, "a.txt")
	if err := os.WriteFile(liveFile, []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(liveFile, baseInfo.ModTime(), baseInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after := scanLiveFingerprint(live).hash; before == after {
		t.Fatalf("live stat hash unchanged after same-size write: %s", after)
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
