package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentStoreRejectsUnavailableReflink(t *testing.T) {
	base, state, bin := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "source"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cp"), []byte("#!/bin/sh\necho 'clone unsupported' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	store, err := NewPersistentStore(base, state)
	if err == nil {
		_ = store.Close()
		t.Fatal("store accepted a filesystem without reflink by copying the project")
	}
	if !strings.Contains(err.Error(), "reflink") {
		t.Fatalf("error = %v, want actionable reflink requirement", err)
	}
	if _, err := os.Stat(filepath.Join(state, floorMetaName)); !os.IsNotExist(err) {
		t.Fatalf("failed store published floor metadata: %v", err)
	}
}

func TestPersistentStoreRejectsEmptyProjectWithoutReflink(t *testing.T) {
	root, err := os.MkdirTemp("/dev/shm", "threadmill-reflink-test-")
	if err != nil {
		t.Skipf("tmpfs fixture unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	base := filepath.Join(root, "project")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStore(base, filepath.Join(root, "state"))
	if err == nil {
		_ = store.Close()
		t.Fatal("empty project bypassed the reflink requirement")
	}
	if !strings.Contains(err.Error(), "reflink") {
		t.Fatalf("error = %v, want reflink requirement", err)
	}
}

// reflinkMount 是本机可选的 reflink 挂载点；存在时验证真实克隆能力，不存在则跳过。
const reflinkMount = "/mnt/threadmill-reflink"

func TestReflinkCloneableOnSameMount(t *testing.T) {
	if _, err := os.Stat(reflinkMount); err != nil {
		t.Skipf("reflink mount %s not present", reflinkMount)
	}
	dir := t.TempDir()
	if ReflinkCloneable(dir, reflinkMount) {
		t.Errorf("cross-device clone should not be possible: base=%s live=%s", dir, reflinkMount)
	}
	if !ReflinkCloneable(reflinkMount, reflinkMount) {
		t.Errorf("same-mount should be cloneable: %s", reflinkMount)
	}
}

func TestMaterializeUsesReflinkOnCloneableMount(t *testing.T) {
	if _, err := os.Stat(reflinkMount); err != nil {
		t.Skipf("reflink mount %s not present", reflinkMount)
	}
	root, err := os.MkdirTemp(reflinkMount, "materialize-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	base := root + "/base"
	liveRoot := root + "/live"
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+"/fixture.txt", []byte("fixture"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStore(base, liveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize("env-a"); err != nil {
		t.Fatal(err)
	}
	stats := store.Stats()
	if stats.MaterializeReflinks != 1 || stats.MaterializeFullCopies != 0 {
		t.Fatalf("materialize stats = %+v, want one reflink", stats)
	}
}

func TestReflinkCloneableMissingPaths(t *testing.T) {
	if ReflinkCloneable("/nonexistent-base", "/nonexistent-live") {
		t.Errorf("missing paths must not report cloneable")
	}
}
