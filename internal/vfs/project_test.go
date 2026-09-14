package vfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectWorkspaceEditsRealFilesAndKeepsArchivesIndependent(t *testing.T) {
	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "file"), "original")
	store, err := NewPersistentStore(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mustCreateEnvironment(t, store, "", "task")
	if err := store.BindProject("task"); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("task")
	if err != nil || live != base {
		t.Fatalf("workspace=%q, %v; want %q", live, err, base)
	}
	if err := store.View("task").Write("file", []byte("real edit")); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "file")); err != nil || string(got) != "real edit" {
		t.Fatalf("real file=%q, %v", got, err)
	}
	if err := store.Archive("task", "output"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("task").Write("file", []byte("later")); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, store.View("output"), "file", "real edit")
	if err := store.Discard("task"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "file")); err != nil || string(got) != "later" {
		t.Fatalf("cleanup damaged project=%q,%v", got, err)
	}
}

func TestProjectReleasePreservesDirectoryAndDetachedTaskState(t *testing.T) {
	base := t.TempDir()
	mustWriteFile(t, filepath.Join(base, "file"), "base")
	store, err := NewPersistentStore(base, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.BindProject("task"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("task").Write("file", []byte("task")); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("task"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(base, "file"), "user")
	assertFileBody(t, store.View("task"), "file", "task")
	if err := store.BindProject("next"); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, store.View("next"), "file", "user")
	if err := store.BindProject("other"); err == nil {
		t.Fatal("multiple environments share writable real directory")
	}
	if err := store.Discard("next"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "file")); err != nil || string(got) != "user" {
		t.Fatalf("real file=%q,%v", got, err)
	}
}

func TestProjectBindingRequiresAnIndependentFloor(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.BindProject("task"); err == nil {
		t.Fatal("mutable project was also used as the shared read floor")
	}
}
