package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSymlinksSurviveArchiveRestoreAndPublish(t *testing.T) {
	for _, overlay := range []bool{false, true} {
		t.Run(fmt.Sprintf("overlay=%v", overlay), func(t *testing.T) {
			base, state := t.TempDir(), t.TempDir()
			mustWriteFile(t, filepath.Join(base, "target.txt"), "target")
			if err := os.Symlink("target.txt", filepath.Join(base, "removed")); err != nil {
				t.Fatal(err)
			}
			store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: overlay})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			mustCreateEnvironment(t, store, "", "task")
			live, err := store.Materialize("task")
			if err != nil {
				t.Fatal(err)
			}
			if overlay && store.Stats().MaterializeOverlays != 1 {
				t.Skip("OverlayFS unavailable")
			}
			links := map[string]string{"alias": "target.txt", "dangling": "missing.txt", "external": "/outside/missing"}
			for name, target := range links {
				if err := os.Symlink(target, filepath.Join(live, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(filepath.Join(live, "removed")); err != nil {
				t.Fatal(err)
			}
			if err := store.Archive("task", "output"); err != nil {
				t.Fatal(err)
			}
			if err := store.Discard("task"); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: overlay})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			if err := restarted.Restore("output"); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Publish("output"); err != nil {
				t.Fatal(err)
			}
			for name, target := range links {
				got, err := os.Readlink(filepath.Join(base, name))
				if err != nil || got != target {
					t.Errorf("published link %s = %q, %v; want %q", name, got, err, target)
				}
			}
			if _, err := os.Lstat(filepath.Join(base, "removed")); !os.IsNotExist(err) {
				t.Errorf("deleted link survived: %v", err)
			}
		})
	}
}

func TestFrozenSymlinksResolveAgainstTheTaskSnapshot(t *testing.T) {
	store, base := newTestStore(t)
	mustWriteFile(t, filepath.Join(base, "dir", "target"), "base")
	if err := os.Symlink("dir", filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("task")
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(live, "dir", "target"), "task")
	if err := os.Symlink("alias/target", filepath.Join(live, "link")); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze("task"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link", "alias/target"} {
		got, err := store.View("task").Read(path)
		if err != nil || string(got) != "task" {
			t.Errorf("Read(%q) = %q, %v; want task", path, got, err)
		}
	}
	if info, err := store.View("task").Stat("alias"); err != nil || !info.IsDir {
		t.Errorf("Stat(alias) = %+v, %v", info, err)
	}
	if entries, err := store.View("task").List("alias"); err != nil || len(entries) != 1 || entries[0].Name != "target" {
		t.Errorf("List(alias) = %+v, %v", entries, err)
	}
}

func TestPublishDirectoryReplacedBySymlink(t *testing.T) {
	base, state := t.TempDir(), t.TempDir()
	mustWriteFile(t, filepath.Join(base, "dir", "old"), "old")
	mustWriteFile(t, filepath.Join(base, "target", "old"), "keep")
	store, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("task")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(live, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(live, "dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish("task"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(base, "dir")); err != nil || got != "target" {
		t.Errorf("dir link = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(base, "target", "old")); err != nil || string(got) != "keep" {
		t.Errorf("target file = %q, %v", got, err)
	}
}

func TestPublishSymlinkReplacedByDirectory(t *testing.T) {
	for _, target := range []string{"target", "/outside/missing"} {
		t.Run(target, func(t *testing.T) {
			base := t.TempDir()
			mustWriteFile(t, filepath.Join(base, "target", "file"), "same")
			if err := os.Symlink(target, filepath.Join(base, "alias")); err != nil {
				t.Fatal(err)
			}
			store, err := NewPersistentStore(base, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			live, err := store.Materialize("task")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(live, "alias")); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, filepath.Join(live, "alias", "file"), "same")
			if _, err := store.Publish("task"); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(base, "alias", "file")); err != nil || string(got) != "same" {
				t.Fatalf("published alias/file = %q, %v", got, err)
			}
		})
	}
}

func TestSymlinkWriteAndDeleteMatchBeforeAndAfterFreeze(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprint(frozen), func(t *testing.T) {
			store, base := newTestStore(t)
			mustWriteFile(t, filepath.Join(base, "dir", "file"), "before")
			live, err := store.Materialize("task")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("dir", filepath.Join(live, "alias")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("dir/file", filepath.Join(live, "link")); err != nil {
				t.Fatal(err)
			}
			if frozen {
				if err := store.Freeze("task"); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.View("task").Write("link", []byte("after")); err != nil {
				t.Fatal(err)
			}
			assertFileBody(t, store.View("task"), "dir/file", "after")
			if err := store.View("task").Delete("alias/file"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.View("task").Read("dir/file"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("target survived deletion: %v", err)
			}
		})
	}
}

func TestFrozenLinkResolvesParentAfterFollowingIntermediateLink(t *testing.T) {
	store, base := newTestStore(t)
	mustWriteFile(t, filepath.Join(base, "elsewhere", "wanted"), "right")
	mustWriteFile(t, filepath.Join(base, "wanted"), "wrong")
	if err := os.Mkdir(filepath.Join(base, "elsewhere", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere/deep", filepath.Join(base, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop/../wanted", filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize("task"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.View("task").Read("alias"); err != nil || string(got) != "right" {
		t.Fatalf("live=%q %v", got, err)
	}
	if err := store.Freeze("task"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.View("task").Read("alias"); err != nil || string(got) != "right" {
		t.Fatalf("frozen=%q %v", got, err)
	}
}

func TestWriteDanglingLink(t *testing.T) {
	for _, materialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "logical", true: "live"}[materialized], func(t *testing.T) {
			store, base := newTestStore(t)
			if err := os.Symlink("missing", filepath.Join(base, "alias")); err != nil {
				t.Fatal(err)
			}
			if materialized {
				if _, err := store.Materialize("task"); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.View("task").Write("alias", []byte("new")); err != nil {
				t.Fatal(err)
			}
			if got, err := store.View("task").Read("missing"); err != nil || string(got) != "new" {
				t.Fatalf("target=%q %v", got, err)
			}
		})
	}
}

func TestPrepareInputPreservesSymlinkTargets(t *testing.T) {
	store, base := newTestStore(t)
	mustWriteFile(t, filepath.Join(base, "one"), "same bytes")
	mustWriteFile(t, filepath.Join(base, "two"), "same bytes")
	for i, id := range []string{"a", "b"} {
		live, err := store.Materialize(id)
		if err != nil {
			t.Fatal(err)
		}
		target := []string{"one", "two"}[i]
		if err := os.Symlink(target, filepath.Join(live, "alias")); err != nil {
			t.Fatal(err)
		}
		if err := store.Freeze(id); err != nil {
			t.Fatal(err)
		}
	}
	input, err := store.PrepareInput("joined", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Paths) != 1 || input.Paths[0] != "alias" {
		t.Fatalf("link targets not compared: %+v", input)
	}
	if _, err := store.ApplyInput(input.Candidates[1], "joined", []string{"alias"}, false); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("joined")
	if err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(live, "alias")); err != nil || target != "two" {
		t.Fatalf("joined link = %q, %v", target, err)
	}
}
