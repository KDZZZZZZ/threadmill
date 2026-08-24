package vfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayMaterializeRecoversAndHandoffs(t *testing.T) {
	if detectOverlayDriver() == nil {
		t.Skip("no usable OverlayFS backend")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "hello.txt"), []byte("base"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "deleted.txt"), []byte("delete me"), 0o640); err != nil {
		t.Fatal(err)
	}

	first, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	live, err := first.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "hello.txt"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if stats := first.Stats(); stats.MaterializeOverlays != 1 || stats.MaterializeFullCopies != 0 {
		t.Fatalf("first materialize stats = %+v, want one overlay", stats)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writeOverlaySeed(first.overlayStatePath("parent"), []overlayFile{{
		path: "recovered-seed.txt",
		b:    blob{data: []byte("seed")},
	}}); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Handoff("parent", "child"); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.View("child").Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "changed" {
		t.Fatalf("recovered child hello.txt = %q, want changed", got)
	}
	if _, err := restarted.View("child").Read("deleted.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered deleted.txt error = %v, want not found", err)
	}
	got, err = restarted.View("child").Read("recovered-seed.txt")
	if err != nil || string(got) != "seed" {
		t.Fatalf("recovered seed = %q, %v, want seed", got, err)
	}
	if err := restarted.Discard("child"); err != nil {
		t.Fatal(err)
	}
}

func TestOverlayMaterializeFallsBackAtCapacity(t *testing.T) {
	if detectOverlayDriver() == nil {
		t.Skip("no usable OverlayFS backend")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "hello.txt"), []byte("base"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStoreWithOptions(
		base,
		state,
		Options{Overlay: true, OverlayLimit: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, envID := range []string{"env-a", "env-b"} {
		if _, err := store.Materialize(envID); err != nil {
			t.Fatal(err)
		}
	}
	stats := store.Stats()
	if stats.MaterializeOverlays != 1 || stats.MaterializeFullCopies != 1 || stats.MaterializeFallbacks != 1 {
		t.Fatalf("materialize stats = %+v, want one overlay and one capacity fallback", stats)
	}
	if stats.OverlayCapacityFallbacks != 1 || stats.OverlayErrorFallbacks != 0 {
		t.Fatalf("overlay fallback classes = %+v, want one capacity fallback", stats)
	}
	if stats.OverlayActive != 1 || stats.OverlayCapacity != 1 {
		t.Fatalf("overlay occupancy = %d/%d, want 1/1", stats.OverlayActive, stats.OverlayCapacity)
	}
}

func TestFuseOverlayAbsorbUsesUpperdirDelta(t *testing.T) {
	driver := detectOverlayDriver()
	if driver == nil || driver.kind != "fuse-overlayfs" {
		t.Skip("fuse-overlayfs unavailable")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"changed.txt": "before",
		"deleted.txt": "delete me",
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "changed.txt"), []byte("after"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "created.txt"), []byte("created"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("parent"); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.AbsorbUpperAttempts != 1 ||
		stats.AbsorbUpperFallbacks != 0 || stats.AbsorbScans != 0 {
		t.Fatalf("absorb stats = %+v, want one FUSE upperdir fast path", stats)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("parent"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"changed.txt": "after",
		"created.txt": "created",
	} {
		got, readErr := store.View("child").Read(path)
		if readErr != nil || string(got) != want {
			t.Fatalf("child %s = %q, %v, want %q", path, got, readErr, want)
		}
	}
	if _, err := store.View("child").Read("deleted.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child deleted.txt error = %v, want not found", err)
	}
}

func TestNativeOverlayAbsorbUsesUpperdirDelta(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native OverlayFS requires root")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "changed.txt"), []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "deleted.txt"), []byte("delete me"), 0o640); err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		path := filepath.Join(base, "fixture", fmt.Sprintf("file-%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().OverlayBackend; got != "native-overlayfs" {
		t.Skipf("native OverlayFS unavailable: backend %q", got)
	}
	if err := os.WriteFile(filepath.Join(live, "changed.txt"), []byte("after"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "created.txt"), []byte("created"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "pure-gone.txt"), []byte("gone"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("parent"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "created.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(live, "created.txt"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "created.txt", "nested.txt"), []byte("nested"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(live, "pure-gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "executable.sh"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("parent"); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("parent"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"changed.txt":            "after",
		"created.txt/nested.txt": "nested",
		"executable.sh":          "#!/bin/sh\n",
	} {
		got, readErr := store.View("child").Read(path)
		if readErr != nil || string(got) != want {
			t.Fatalf("child %s = %q, %v, want %q", path, got, readErr, want)
		}
	}
	if _, err := store.View("child").Read("deleted.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child deleted.txt error = %v, want not found", err)
	}
	if _, err := store.View("child").Read("pure-gone.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child pure-gone.txt error = %v, want pure-upper deletion", err)
	}
	childLive, err := store.Materialize("child")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(childLive, "executable.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("child executable mode = %v, %v, want executable", info, err)
	}
	if stats := store.Stats(); stats.AbsorbScans != 0 ||
		stats.AbsorbFastPaths != 3 ||
		stats.AbsorbUpperAttempts != 3 ||
		stats.AbsorbUpperEntries == 0 ||
		stats.AbsorbUpperFallbacks != 0 ||
		stats.AbsorbUpperErrors != 0 ||
		stats.AbsorbUpperDuration <= 0 {
		t.Fatalf("absorb stats = %+v, want three upperdir fast paths and no merged-tree scans", stats)
	}
}

func TestOverlayAbsorbFallsBackForOpaqueDirectory(t *testing.T) {
	if detectOverlayDriver() == nil {
		t.Skip("no usable OverlayFS backend")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(filepath.Join(base, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "dir", "old.txt"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	live, err := store.Materialize("parent")
	if err != nil {
		t.Fatal(err)
	}
	if store.Stats().MaterializeOverlays != 1 {
		t.Skip("native OverlayFS unavailable")
	}
	if err := os.RemoveAll(filepath.Join(live, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(live, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "dir", "new.txt"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("parent"); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := store.Discard("parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.View("child").Read("dir/old.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child dir/old.txt error = %v, want not found", err)
	}
	got, err := store.View("child").Read("dir/new.txt")
	if err != nil || string(got) != "new" {
		t.Fatalf("child dir/new.txt = %q, %v, want new", got, err)
	}
	stats := store.Stats()
	if stats.AbsorbUpperFallbacks == 0 || stats.AbsorbScans != 1 {
		t.Fatalf("absorb stats = %+v, want conservative upperdir fallback and one merged scan", stats)
	}
}
