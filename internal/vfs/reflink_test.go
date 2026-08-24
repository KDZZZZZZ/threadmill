package vfs

import (
	"os"
	"testing"
)

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
