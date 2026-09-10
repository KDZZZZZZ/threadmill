package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPrepareInputSingleSourceReusesCompleteIndependentState(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustCreateEnvironment(t, store, "", "ancestor")
	if err := store.View("ancestor").Write("inherited.txt", []byte("inherited")); err != nil {
		t.Fatal(err)
	}
	mustCreateEnvironment(t, store, "ancestor", "source")
	if err := store.View("source").Write("local.txt", []byte("source")); err != nil {
		t.Fatal(err)
	}
	input, err := store.PrepareInput("target", []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Paths) != 0 || len(input.Candidates) != 0 {
		t.Fatalf("PrepareInput() = %+v, want no differences", input)
	}
	assertFileBody(t, store.View("target"), "hello.txt", "hello")
	assertFileBody(t, store.View("target"), "inherited.txt", "inherited")
	assertFileBody(t, store.View("target"), "local.txt", "source")
	if err := store.View("target").Write("local.txt", []byte("target")); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, store.View("source"), "local.txt", "source")
	if err := store.View("source").Write("inherited.txt", []byte("later")); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, store.View("target"), "inherited.txt", "inherited")
}

func TestPrepareInputComparesAllVisibleFilesAndExposesAbsence(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustCreateEnvironment(t, store, "", "ancestor")
	if err := store.View("ancestor").Write("inherited.txt", []byte("old but unique")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("ancestor").Write("shared.txt", []byte("common")); err != nil {
		t.Fatal(err)
	}
	mustCreateEnvironment(t, store, "ancestor", "a")
	if err := store.View("a").Delete("hello.txt"); err != nil {
		t.Fatal(err)
	}
	mustCreateEnvironment(t, store, "", "b")
	if err := store.View("b").Write("shared.txt", []byte("common")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("b").Write("only-b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}

	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hello.txt", "inherited.txt", "only-b.txt"}
	if !slices.Equal(input.Paths, want) || len(input.Candidates) != 2 {
		t.Fatalf("PrepareInput() = %+v, want paths %v and two candidates", input, want)
	}
	assertFileBody(t, store.View("target"), "shared.txt", "common")
	for _, path := range want {
		if _, err := store.View("target").Read(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("common state contains %s: %v", path, err)
		}
	}
	for _, id := range input.Candidates {
		changes, err := store.InputChanges(id)
		if err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, len(changes))
		for _, change := range changes {
			paths = append(paths, change.Path)
		}
		if !slices.Equal(paths, want) {
			t.Fatalf("candidate changes = %v, want explicit state for %v", changes, want)
		}
	}
	result, err := store.ApplyInput(input.Candidates[0], "target", []string{"inherited.txt"}, false)
	if err != nil || len(result.Conflicts) != 0 {
		t.Fatalf("apply inherited source file: %+v, %v", result, err)
	}
	assertFileBody(t, store.View("target"), "inherited.txt", "old but unique")
	result, err = store.ApplyInput(input.Candidates[1], "target", nil, true)
	if err != nil || len(result.Conflicts) != 0 {
		t.Fatalf("apply b complete differences: %+v, %v", result, err)
	}
	assertFileBody(t, store.View("target"), "hello.txt", "hello")
	assertFileBody(t, store.View("target"), "only-b.txt", "b")
	if _, err := store.View("target").Read("inherited.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("explicit absence did not delete inherited.txt: %v", err)
	}
	assertFileBody(t, store.View("a"), "inherited.txt", "old but unique")
}

func TestPrepareInputRetryPreservesDecisionsAndRejectsDifferentSources(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustCreateEnvironment(t, store, "", "a")
	mustCreateEnvironment(t, store, "", "b")
	if err := store.View("a").Write("hello.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	first, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View("target").Write("hello.txt", []byte("resolved")); err != nil {
		t.Fatal(err)
	}
	again, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Paths, first.Paths) || !slices.Equal(again.Candidates, first.Candidates) {
		t.Fatalf("retry = %+v, want %+v", again, first)
	}
	assertFileBody(t, store.View("target"), "hello.txt", "resolved")
	if _, err := store.PrepareInput("target", []string{"b", "a"}); err == nil {
		t.Fatal("changed source set reused an existing input")
	}
	assertFileBody(t, store.View("target"), "hello.txt", "resolved")
}

func TestPrepareInputInvalidSourcesNeverCreatePartialTarget(t *testing.T) {
	t.Parallel()

	for _, sources := range [][]string{nil, {""}, {"a", "missing"}, {"a", "a"}, {"a", "target"}} {
		t.Run(fmt.Sprint(sources), func(t *testing.T) {
			store, _ := newTestStore(t)
			mustCreateEnvironment(t, store, "", "a")
			if _, err := store.PrepareInput("target", sources); err == nil {
				t.Fatal("invalid input succeeded")
			}
			if err := store.Restore("target"); !errors.Is(err, ErrUnknownEnvironment) {
				t.Fatalf("target exists after failure: %v", err)
			}
		})
	}
}

func TestPrepareInputSafeAbsenceConflictsWithAcceptedFile(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustCreateEnvironment(t, store, "", "a")
	mustCreateEnvironment(t, store, "", "b")
	if err := store.View("a").Write("only-a.txt", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("b").Write("only-b.txt", []byte("b")); err != nil {
		t.Fatal(err)
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyInput(input.Candidates[0], "target", []string{"only-a.txt"}, false); err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyInput(input.Candidates[1], "target", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.Conflicts, []string{"only-a.txt"}) || len(result.Applied) != 0 {
		t.Fatalf("ApplyInput() = %+v, want atomic conflict for explicit absence", result)
	}
	assertFileBody(t, store.View("target"), "only-a.txt", "a")
	if _, err := store.View("target").Read("only-b.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("conflicting apply wrote only-b.txt: %v", err)
	}
}

func TestPrepareInputComparesAndPreservesAllPermissionBits(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	for i, source := range []string{"a", "b"} {
		mustCreateEnvironment(t, store, "", source)
		live, err := store.Materialize(source)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(live, "permissions.txt")
		if err := os.WriteFile(path, []byte("same bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, []fs.FileMode{0o640, 0o660}[i]); err != nil {
			t.Fatal(err)
		}
		if err := store.Release(source); err != nil {
			t.Fatal(err)
		}
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(input.Paths, []string{"permissions.txt"}) {
		t.Fatalf("Paths = %v, want permission-only difference", input.Paths)
	}
	if _, err := store.ApplyInput(input.Candidates[1], "target", nil, false); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("target")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(live, "permissions.txt"))
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("selected file mode = %v, %v, want 0660", info, err)
	}
}

func TestPrepareInputIncludesDirectoryStatesAndFileReplacement(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustCreateEnvironment(t, store, "", "a")
	mustCreateEnvironment(t, store, "", "b")
	live, err := store.Materialize("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(live, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := store.View("a").Write("item/child.txt", []byte("child")); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("b").Write("item", []byte("file")); err != nil {
		t.Fatal(err)
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"empty", "item", "item/child.txt"}
	if !slices.Equal(input.Paths, want) {
		t.Fatalf("Paths = %v, want %v", input.Paths, want)
	}
	for _, path := range []string{"empty", "item"} {
		if _, err := store.View("target").Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("common state contains differing directory %q: %v", path, err)
		}
	}
	result, err := store.ApplyInput(input.Candidates[0], "target", nil, false)
	if err != nil || len(result.Conflicts) != 0 {
		t.Fatalf("apply directory candidate: %+v, %v", result, err)
	}
	info, err := store.View("target").Stat("empty")
	if err != nil || !info.IsDir {
		t.Fatalf("selected empty directory = %+v, %v", info, err)
	}
	assertFileBody(t, store.View("target"), "item/child.txt", "child")
	result, err = store.ApplyInput(input.Candidates[1], "target", nil, true)
	if err != nil || len(result.Conflicts) != 0 {
		t.Fatalf("replace directory with file: %+v, %v", result, err)
	}
	assertFileBody(t, store.View("target"), "item", "file")
	if _, err := store.View("target").Stat("empty"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted empty directory still exists: %v", err)
	}
}

func TestPrepareInputIdenticalSourcesNeedNoDecisionsAfterMaterialization(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	if err := os.Mkdir(filepath.Join(base, "common-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustCreateEnvironment(t, store, "", "a")
	mustCreateEnvironment(t, store, "", "b")
	if _, err := store.Materialize("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("a"); err != nil {
		t.Fatal(err)
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Paths) != 0 || len(input.Candidates) != 0 {
		t.Fatalf("identical source states produced differences: %+v", input)
	}
	info, err := store.View("target").Stat("common-dir")
	if err != nil || !info.IsDir {
		t.Fatalf("common directory = %+v, %v", info, err)
	}
}

func TestPrepareInputRecoversCandidatesAndDecisionsAfterRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	store, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"a", "b"} {
		mustCreateEnvironment(t, store, "", source)
		if err := store.View(source).Write("choice.txt", []byte(source)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.View("target").Write("choice.txt", []byte("resolved")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	again, err := restarted.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Paths, first.Paths) || !slices.Equal(again.Candidates, first.Candidates) {
		t.Fatalf("recovered input = %+v, want %+v", again, first)
	}
	assertFileBody(t, restarted.View("target"), "choice.txt", "resolved")
	for i, id := range again.Candidates {
		assertFileBody(t, restarted.View(id), "choice.txt", []string{"a", "b"}[i])
	}
	result, err := restarted.ApplyInput(again.Candidates[0], "target", nil, false)
	if err != nil || !slices.Equal(result.Conflicts, []string{"choice.txt"}) {
		t.Fatalf("safe apply after resume = %+v, %v, want existing decision protected", result, err)
	}
}

func TestPrepareInputDirectoryPermissionDifferenceKeepsCommonChildren(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	if err := os.Mkdir(filepath.Join(base, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(base, "shared", "kept.txt"), "common")
	for i, source := range []string{"a", "b"} {
		mustCreateEnvironment(t, store, "", source)
		live, err := store.Materialize(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(live, "shared"), []fs.FileMode{0o750, 0o755}[i]); err != nil {
			t.Fatal(err)
		}
		if err := store.Release(source); err != nil {
			t.Fatal(err)
		}
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(input.Paths, []string{"shared"}) {
		t.Fatalf("Paths = %v, want only directory permissions", input.Paths)
	}
	assertFileBody(t, store.View("target"), "shared/kept.txt", "common")
	if _, err := store.ApplyInput(input.Candidates[1], "target", nil, false); err != nil {
		t.Fatal(err)
	}
	assertFileBody(t, store.View("target"), "shared/kept.txt", "common")
}

func TestPrepareInputRetainsOverlayDirectoryMetadata(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewPersistentStoreWithOptions(base, state, Options{Overlay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	mustCreateEnvironment(t, store, "", "a")
	mustCreateEnvironment(t, store, "", "b")
	live, err := store.Materialize("a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(live, "existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Absorb("a"); err != nil {
		t.Fatal(err)
	}
	input, err := store.PrepareInput("target", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(input.Paths, []string{"existing"}) {
		t.Fatalf("Paths = %v, want changed directory permissions", input.Paths)
	}
	if _, err := store.ApplyInput(input.Candidates[0], "target", nil, false); err != nil {
		t.Fatal(err)
	}
	target, err := store.Materialize("target")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(target, "existing"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("selected directory = %v, %v, want mode 0700", info, err)
	}
}
