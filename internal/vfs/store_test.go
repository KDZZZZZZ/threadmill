package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestViewRejectsEscapingPaths(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")

	ops := []struct {
		name string
		call func(path string) error
	}{
		{"read", func(path string) error { _, err := view.Read(path); return err }},
		{"write", func(path string) error { return view.Write(path, []byte("x")) }},
		{"delete", func(path string) error { return view.Delete(path) }},
		{"stat", func(path string) error { _, err := view.Stat(path); return err }},
		{"list", func(path string) error { _, err := view.List(path); return err }},
	}
	paths := []string{"..", "../secret.txt", "/etc/passwd", "foo/../../etc/passwd"}

	for _, op := range ops {
		op := op
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			for _, path := range paths {
				if err := op.call(path); err == nil {
					t.Fatalf("%s(%q) succeeded, want path jail error", op.name, path)
				}
			}
		})
	}
}

func TestViewJailCleansDotDotInsideRoot(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")

	got, err := view.Read("sub/../hello.txt")
	if err != nil {
		t.Fatalf("read cleaned path: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("sub/../hello.txt = %q, want hello", got)
	}
}

func TestViewWritesStayIsolatedAcrossEnvs(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	viewA := store.View("env-a")
	viewB := store.View("env-b")

	if err := viewA.Write("hello.txt", []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	if err := viewA.Write("only-a.txt", []byte("secret")); err != nil {
		t.Fatal(err)
	}

	gotB, err := viewB.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotB) != "hello" {
		t.Fatalf("env-b hello.txt = %q, want base hello", gotB)
	}
	if _, err := viewB.Read("only-a.txt"); err == nil {
		t.Fatal("env-b saw env-a overlay file")
	}

	gotA, err := viewA.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotA) != "from-a" {
		t.Fatalf("env-a hello.txt = %q, want from-a", gotA)
	}
}

func TestViewDeleteTombsBaseFile(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	view := store.View("env-a")

	if err := view.Delete("hello.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Read("hello.txt"); err == nil {
		t.Fatal("tombstone still exposed the base file")
	}
	if _, err := view.Stat("hello.txt"); err == nil {
		t.Fatal("stat saw a tombstoned base file")
	}

	host, err := os.ReadFile(filepath.Join(base, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(host) != "hello" {
		t.Fatalf("base hello.txt = %q, want untouched hello", host)
	}

	other := store.View("env-b")
	got, err := other.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("env-b hello.txt = %q, want base hello", got)
	}
}

func TestStoreForkIsolatesChildWritesAndInheritsReads(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("from-parent.txt", []byte("parent-blob")); err != nil {
		t.Fatal(err)
	}

	store.Fork("parent", "child")
	child := store.View("child")

	gotBase, err := child.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBase) != "hello" {
		t.Fatalf("child hello.txt = %q, want base hello", gotBase)
	}
	gotParent, err := child.Read("from-parent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotParent) != "parent-blob" {
		t.Fatalf("child from-parent.txt = %q, want parent-blob", gotParent)
	}

	if err := child.Write("from-parent.txt", []byte("child-blob")); err != nil {
		t.Fatal(err)
	}
	if err := child.Write("from-child.txt", []byte("only-child")); err != nil {
		t.Fatal(err)
	}

	stillParent, err := parent.Read("from-parent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(stillParent) != "parent-blob" {
		t.Fatal("child write leaked into parent")
	}
	if _, err := parent.Read("from-child.txt"); err == nil {
		t.Fatal("parent saw child overlay file")
	}

	gotChild, err := child.Read("from-parent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotChild) != "child-blob" {
		t.Fatalf("child from-parent.txt = %q, want child-blob", gotChild)
	}
}

func TestStoreForkDoesNotOverwriteExistingChild(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	child := store.View("child")
	if err := child.Write("kept.txt", []byte("existing")); err != nil {
		t.Fatal(err)
	}

	store.Fork("parent", "child")

	got, err := store.View("child").Read("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatalf("child kept.txt = %q, want existing", got)
	}
}

func TestStoreMergeAppliesChildWriteAndKeepsParentWrite(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("only-parent.txt", []byte("parent-only")); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	child := store.View("child")
	if err := child.Write("only-child.txt", []byte("child-only")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	gotChild, err := parent.Read("only-child.txt")
	if err != nil {
		t.Fatalf("parent missing child write: %v", err)
	}
	if string(gotChild) != "child-only" {
		t.Fatalf("parent only-child.txt = %q, want child-only", gotChild)
	}
	gotParent, err := parent.Read("only-parent.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotParent) != "parent-only" {
		t.Fatalf("parent only-parent.txt = %q, want parent-only", gotParent)
	}
	if _, err := store.View("child").Read("only-parent.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreMergeAppliesChildTombstone(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("from-parent.txt", []byte("parent-blob")); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	child := store.View("child")
	if err := child.Delete("hello.txt"); err != nil {
		t.Fatal(err)
	}
	if err := child.Delete("from-parent.txt"); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if _, err := parent.Read("hello.txt"); err == nil {
		t.Fatal("merged tombstone still exposed the base file")
	}
	if _, err := parent.Read("from-parent.txt"); err == nil {
		t.Fatal("merged tombstone still exposed the parent overlay file")
	}
	host, err := os.ReadFile(filepath.Join(base, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(host) != "hello" {
		t.Fatalf("base hello.txt = %q, want untouched hello", host)
	}
}

func TestStoreMergeConflictsWhenBothSidesWrote(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	store.Fork("parent", "child")
	if err := parent.Write("conflict.txt", []byte("ours")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("keep.txt", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	child := store.View("child")
	if err := child.Write("conflict.txt", []byte("theirs")); err != nil {
		t.Fatal(err)
	}
	if err := child.Write("extra.txt", []byte("child-extra")); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "parent")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	if !strings.Contains(err.Error(), "conflict.txt") {
		t.Fatalf("conflict error = %v, want path conflict.txt", err)
	}

	got, readErr := parent.Read("conflict.txt")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "ours" {
		t.Fatalf("parent conflict.txt = %q, want ours", got)
	}
	if _, err := parent.Read("extra.txt"); err == nil {
		t.Fatal("conflict Merge applied remaining child paths")
	}
	keep, err := parent.Read("keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(keep) != "keep" {
		t.Fatalf("parent keep.txt = %q, want keep", keep)
	}
}

func TestStoreMergeReplayDoesNotError(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	store.Fork("parent", "child")
	if err := store.View("child").Write("from-child.txt", []byte("only-child")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("second Merge: %v", err)
	}

	got, err := store.View("parent").Read("from-child.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "only-child" {
		t.Fatalf("parent from-child.txt = %q, want only-child", got)
	}
}

func TestStoreMergeConflictsWhenGrandparentChangedAfterNestedFork(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	gp := store.View("gp")
	if err := gp.Write("shared.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	store.Fork("gp", "parent")
	store.Fork("parent", "child")
	if err := gp.Write("shared.txt", []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Write("shared.txt", []byte("C")); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "gp")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Fatalf("conflict error = %v, want path shared.txt", err)
	}
	got, readErr := gp.Read("shared.txt")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "B" {
		t.Fatalf("gp shared.txt = %q, want B", got)
	}
}

func TestStoreMergeAppliesChildWriteUnderParentDirTombstone(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	if err := store.View("child").Write("dir/new.txt", []byte("recreated")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := parent.Read("dir/new.txt")
	if err != nil {
		t.Fatalf("parent missing child recreate: %v", err)
	}
	if string(got) != "recreated" {
		t.Fatalf("parent dir/new.txt = %q, want recreated", got)
	}
	info, err := parent.Stat("dir")
	if err != nil {
		t.Fatalf("Stat dir after recreate: %v", err)
	}
	if !info.IsDir {
		t.Fatal("Stat dir after recreate: want directory")
	}
	ents, err := parent.List("dir")
	if err != nil {
		t.Fatalf("List dir after recreate: %v", err)
	}
	if len(ents) != 1 || ents[0].Name != "new.txt" || ents[0].IsDir {
		t.Fatalf("List dir = %#v, want [new.txt file]", ents)
	}
}

func TestStoreMergeRecreateDirKeepsMaskedHostDescendantsHidden(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Delete("sub"); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	if err := store.View("child").Write("sub/new.txt", []byte("recreated")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := parent.Read("sub/new.txt")
	if err != nil {
		t.Fatalf("parent missing child recreate: %v", err)
	}
	if string(got) != "recreated" {
		t.Fatalf("parent sub/new.txt = %q, want recreated", got)
	}
	if _, err := parent.Read("sub/nested.txt"); err == nil {
		t.Fatal("merge resurrected host sub/nested.txt")
	}
	ents, err := parent.List("sub")
	if err != nil {
		t.Fatalf("List sub: %v", err)
	}
	if len(ents) != 1 || ents[0].Name != "new.txt" {
		t.Fatalf("List sub = %#v, want [new.txt]", ents)
	}
}

func TestStoreMergeOverlappingDirTombstoneAndChildWriteIsDeterministic(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Delete("sub"); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	child := store.View("child")
	if err := child.Write("sub/x.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if err := child.Delete("sub"); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := parent.Read("sub/x.txt")
	if err != nil {
		t.Fatalf("overlapping child write missing after Merge: %v", err)
	}
	if string(got) != "kept" {
		t.Fatalf("parent sub/x.txt = %q, want kept", got)
	}
	if _, err := parent.Read("sub/nested.txt"); err == nil {
		t.Fatal("merge resurrected host sub/nested.txt")
	}
}

func TestStoreMergeConflictsWhenChildDeletesDirOverChangedDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("dir/file.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	if err := parent.Write("dir/file.txt", []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Delete("dir"); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "parent")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	if !strings.Contains(err.Error(), "dir/file.txt") && !strings.Contains(err.Error(), "dir") {
		t.Fatalf("conflict error = %v, want dir or dir/file.txt", err)
	}
	got, readErr := parent.Read("dir/file.txt")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "B" {
		t.Fatalf("parent dir/file.txt = %q, want B", got)
	}
}

func TestStoreMergeChildDirTombstoneHidesUnchangedDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("dir/file.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	if err := store.View("child").Delete("dir"); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := parent.Read("dir/file.txt"); err == nil {
		t.Fatal("child dir tombstone left parent dir/file.txt visible")
	}
	if _, err := parent.List("dir"); err == nil {
		t.Fatal("List still showed tombstoned dir via inherited overlay children")
	}
}

func TestStoreMergeConflictsWhenMatchingDirTombstoneHidesTargetDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	store.Fork("parent", "child")
	if err := parent.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Delete("dir"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("dir/new.txt", []byte("ours")); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "parent")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	got, readErr := parent.Read("dir/new.txt")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "ours" {
		t.Fatalf("parent dir/new.txt = %q, want ours", got)
	}
}

func TestStoreMergeConflictsWhenTargetReplacesTombstonedDirWithFile(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	if err := parent.Write("dir", []byte("now-a-file")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Write("dir/x.txt", []byte("from-child")); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "parent")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	got, readErr := parent.Read("dir")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "now-a-file" {
		t.Fatalf("parent dir = %q, want now-a-file", got)
	}
}

func TestStoreMergeConflictsWhenGrandparentChangedDirDescendantAfterNestedFork(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	gp := store.View("gp")
	if err := gp.Write("dir/file.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	store.Fork("gp", "parent")
	store.Fork("parent", "child")
	if err := gp.Write("dir/file.txt", []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Delete("dir"); err != nil {
		t.Fatal(err)
	}

	err := store.Merge("child", "gp")
	if err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
	got, readErr := gp.Read("dir/file.txt")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "B" {
		t.Fatalf("gp dir/file.txt = %q, want B", got)
	}
}

func TestStoreMergeEmptyIntoIsNoop(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	store.Fork("parent", "child")
	if err := store.View("child").Write("from-child.txt", []byte("only-child")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", ""); err != nil {
		t.Fatalf("Merge into empty: %v", err)
	}
	if _, err := store.View("parent").Read("from-child.txt"); err == nil {
		t.Fatal("empty into applied onto parent")
	}
}

func TestViewReadReturnsCopiedBlob(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")

	base, err := view.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	base[0] = 'X'
	gotBase, err := view.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBase) != "hello" {
		t.Fatal("mutating base Read changed the store")
	}

	if err := view.Write("hello.txt", []byte("overlay")); err != nil {
		t.Fatal(err)
	}
	overlay, err := view.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	overlay[0] = 'Z'
	gotOverlay, err := view.Read("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOverlay) != "overlay" {
		t.Fatal("mutating overlay Read changed the store")
	}
}

func TestViewRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	store, base := newTestStore(t)
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "leak")
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	view := store.View("env-a")

	if _, err := view.Read("link/secret.txt"); err == nil {
		t.Fatal("Read followed a symlink out of base")
	}
	if _, err := view.Stat("link/secret.txt"); err == nil {
		t.Fatal("Stat followed a symlink out of base")
	}
	if _, err := view.List("link"); err == nil {
		t.Fatal("List followed a symlink out of base")
	}
}

func TestViewChildDirTombstoneHidesParentOverlayChildren(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("parent").Write("dir/old.txt", []byte("from-parent")); err != nil {
		t.Fatal(err)
	}
	store.Fork("parent", "child")
	child := store.View("child")
	if err := child.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Stat("dir"); err == nil {
		t.Fatal("Stat saw parent overlay children under child dir tombstone")
	}
	if _, err := child.List("dir"); err == nil {
		t.Fatal("List saw parent overlay children under child dir tombstone")
	}
	ents, err := store.View("parent").List("dir")
	if err != nil {
		t.Fatal(err)
	}
	if !dirEntNamed(ents, "old.txt") {
		t.Fatalf("parent List(dir) = %#v, want old.txt", ents)
	}
}

func TestViewAncestorTombstoneHidesDescendants(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")
	if err := view.Delete("sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Read("sub/nested.txt"); err == nil {
		t.Fatal("Read saw a file under a tombstoned directory")
	}
	if _, err := view.Stat("sub/nested.txt"); err == nil {
		t.Fatal("Stat saw a file under a tombstoned directory")
	}
	if _, err := view.Stat("sub"); err == nil {
		t.Fatal("Stat saw tombstoned directory via inherited children")
	}
	if _, err := view.List("sub"); err == nil {
		t.Fatal("List saw tombstoned directory via inherited children")
	}

	if err := view.Write("sub", []byte("now-a-file")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Read("sub/nested.txt"); err == nil {
		t.Fatal("Read saw a directory child after replacing the directory with a file")
	}
}

func TestViewListKeepsDirAfterNestedTombstone(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")
	if err := view.Delete("sub/nested.txt"); err != nil {
		t.Fatal(err)
	}

	ents, err := view.List(".")
	if err != nil {
		t.Fatal(err)
	}
	if !dirEntNamed(ents, "sub") {
		t.Fatal("List(\".\") hid sub after tombstoning only sub/nested.txt")
	}
	if dirEntNamed(ents, "nested.txt") {
		t.Fatal("List(\".\") listed a nested file as a root entry")
	}

	children, err := view.List("sub")
	if err != nil {
		t.Fatal(err)
	}
	if dirEntNamed(children, "nested.txt") {
		t.Fatal("List(\"sub\") still showed the tombstoned file")
	}
}

func TestViewListRootShowsRecreatedDirAfterSameLayerTombstone(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")
	if err := view.Delete("sub"); err != nil {
		t.Fatal(err)
	}
	if err := view.Write("sub/x.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		ents, err := view.List(".")
		if err != nil {
			t.Fatalf("List(.): %v", err)
		}
		if !dirEntNamed(ents, "sub") {
			t.Fatalf("List(.) missing recreated sub on iteration %d: %#v", i, ents)
		}
	}
	got, err := view.Read("sub/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "kept" {
		t.Fatalf("sub/x.txt = %q, want kept", got)
	}
}

func TestViewListOverlayChildrenOverBaseFile(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	view := store.View("env-a")
	if err := view.Write("hello.txt/child.txt", []byte("inside")); err != nil {
		t.Fatal(err)
	}

	ents, err := view.List("hello.txt")
	if err != nil {
		t.Fatalf("List hello.txt as overlay dir: %v", err)
	}
	if !dirEntNamed(ents, "child.txt") {
		t.Fatalf("List(hello.txt) = %#v, want child.txt", ents)
	}
}

func dirEntNamed(ents []DirEnt, name string) bool {
	for _, ent := range ents {
		if ent.Name == name {
			return true
		}
	}
	return false
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "hello.txt"), "hello")
	mustWriteFile(t, filepath.Join(dir, "sub", "nested.txt"), "nested")
	return NewStore(dir), dir
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
