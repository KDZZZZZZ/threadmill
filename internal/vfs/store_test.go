package vfs

import (
	"encoding/json"
	"errors"
	"io/fs"
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

	mustFork(t, store, "parent", "child")
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

	mustFork(t, store, "parent", "child")
	got, err := store.View("child").Read("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatalf("child kept.txt = %q, want existing", got)
	}
}

func TestStoreForkIsFrozenSnapshot(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("initial.txt", []byte("init")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("mod.txt", []byte("v1")); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "parent", "child")
	child := store.View("child")

	// Parent modifies state after fork.
	if err := parent.Write("parent-after.txt", []byte("after")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("mod.txt", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Delete("initial.txt"); err != nil {
		t.Fatal(err)
	}

	// Child must see the snapshot at fork time.
	gotInit, err := child.Read("initial.txt")
	if err != nil {
		t.Fatalf("child failed to read initial.txt: %v", err)
	}
	if string(gotInit) != "init" {
		t.Fatalf("child initial.txt = %q, want init", gotInit)
	}

	gotMod, err := child.Read("mod.txt")
	if err != nil {
		t.Fatalf("child failed to read mod.txt: %v", err)
	}
	if string(gotMod) != "v1" {
		t.Fatalf("child mod.txt = %q, want v1 (saw parent's post-fork modification)", gotMod)
	}

	if _, err := child.Read("parent-after.txt"); err == nil {
		t.Fatal("child saw parent's post-fork file parent-after.txt")
	}
	if _, err := child.Stat("parent-after.txt"); err == nil {
		t.Fatal("child stat saw parent's post-fork file parent-after.txt")
	}

	ents, err := child.List(".")
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, e := range ents {
		names[e.Name] = true
	}
	if names["parent-after.txt"] {
		t.Fatal("child list included parent-after.txt")
	}
	if !names["initial.txt"] {
		t.Fatal("child list missing initial.txt")
	}
	if !names["mod.txt"] {
		t.Fatal("child list missing mod.txt")
	}
}

func TestStoreForkNestedIsFrozenSnapshot(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	gp := store.View("gp")
	if err := gp.Write("gp.txt", []byte("g1")); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "gp", "parent")
	parent := store.View("parent")
	if err := parent.Write("parent.txt", []byte("p1")); err != nil {
		t.Fatal(err)
	}

	mustFork(t, store, "parent", "child")
	child := store.View("child")

	// Post-fork mutations to gp and parent.
	if err := gp.Write("gp.txt", []byte("g2")); err != nil {
		t.Fatal(err)
	}
	if err := gp.Write("gp-after.txt", []byte("gafter")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("parent.txt", []byte("p2")); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("parent-after.txt", []byte("pafter")); err != nil {
		t.Fatal(err)
	}

	// Child verifies isolated frozen chain.
	gotGP, err := child.Read("gp.txt")
	if err != nil || string(gotGP) != "g1" {
		t.Fatalf("child gp.txt = %q (err %v), want g1", gotGP, err)
	}
	gotP, err := child.Read("parent.txt")
	if err != nil || string(gotP) != "p1" {
		t.Fatalf("child parent.txt = %q (err %v), want p1", gotP, err)
	}
	if _, err := child.Read("gp-after.txt"); err == nil {
		t.Fatal("child saw gp-after.txt")
	}
	if _, err := child.Read("parent-after.txt"); err == nil {
		t.Fatal("child saw parent-after.txt")
	}

	// Parent verifies isolated frozen chain.
	parentGP, err := parent.Read("gp.txt")
	if err != nil || string(parentGP) != "g1" {
		t.Fatalf("parent gp.txt = %q (err %v), want g1", parentGP, err)
	}
	if _, err := parent.Read("gp-after.txt"); err == nil {
		t.Fatal("parent saw gp-after.txt")
	}
}

func TestStoreMergeAppliesChildWriteAndKeepsParentWrite(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("only-parent.txt", []byte("parent-only")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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

func TestStorePrepareMergeLetsTargetFilterAndResolveFiles(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	target := store.View("target")
	if err := target.Write("shared.txt", []byte("base")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "target", "child")
	if err := target.Write("shared.txt", []byte("ours")); err != nil {
		t.Fatal(err)
	}
	child := store.View("child")
	if err := child.Write("shared.txt", []byte("theirs")); err != nil {
		t.Fatal(err)
	}
	if err := child.Write("optional.txt", []byte("optional")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "target", "join")

	manifest, err := store.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}})
	if err != nil {
		t.Fatalf("PrepareMerge: %v", err)
	}
	if len(manifest.Changes) != 2 {
		t.Fatalf("changes = %#v, want shared and optional", manifest.Changes)
	}
	joined := store.View("join")
	got, err := joined.Read("optional.txt")
	if err != nil || string(got) != "optional" {
		t.Fatalf("auto-merged optional.txt = %q, %v", got, err)
	}
	got, err = joined.Read("shared.txt")
	if err != nil || string(got) != "ours" {
		t.Fatalf("conflicted shared.txt = %q, %v; want ours", got, err)
	}
	got, err = joined.Read(MergeRuntimeDir + "/sources/source-1/shared.txt")
	if err != nil || string(got) != "theirs" {
		t.Fatalf("source shared.txt = %q, %v; want theirs", got, err)
	}
	got, err = joined.Read(MergeRuntimeDir + "/ours/source-1/shared.txt")
	if err != nil || string(got) != "ours" {
		t.Fatalf("ours shared.txt = %q, %v; want ours", got, err)
	}
	data, err := joined.Read(MergeRuntimeDir + "/manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var persisted MergeManifest
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(persisted.Changes) != len(manifest.Changes) {
		t.Fatalf("persisted changes = %#v, want %#v", persisted.Changes, manifest.Changes)
	}

	if err := joined.Delete("optional.txt"); err != nil {
		t.Fatal(err)
	}
	if err := joined.Write("shared.txt", []byte("resolved")); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitMerge("join", "target"); err != nil {
		t.Fatalf("CommitMerge: %v", err)
	}
	got, err = target.Read("shared.txt")
	if err != nil || string(got) != "resolved" {
		t.Fatalf("target shared.txt = %q, %v; want resolved", got, err)
	}
	if _, err := target.Read("optional.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("excluded optional.txt survived: %v", err)
	}
	if _, err := target.Stat(MergeRuntimeDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("merge runtime leaked into target: %v", err)
	}
}

func TestStorePrepareMergeHandlesFileDirectoryConflict(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	if err := store.View("target").Write("dir", []byte("ours")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "target", "child")
	if err := store.View("child").Write("dir/file.txt", []byte("theirs")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "target", "join")

	manifest, err := store.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}})
	if err != nil {
		t.Fatalf("PrepareMerge: %v", err)
	}
	if len(manifest.Changes) != 1 || manifest.Changes[0].Status != "conflict" {
		t.Fatalf("changes = %#v, want file/directory conflict", manifest.Changes)
	}
	got, err := store.View("join").Read(MergeRuntimeDir + "/sources/source-1/dir/file.txt")
	if err != nil || string(got) != "theirs" {
		t.Fatalf("source dir/file.txt = %q, %v; want theirs", got, err)
	}
	got, err = store.View("join").Read(MergeRuntimeDir + "/ours/source-1/dir")
	if err != nil || string(got) != "ours" {
		t.Fatalf("ours blocking dir = %q, %v; want ours", got, err)
	}
	got, err = store.View("join").Read("dir")
	if err != nil || string(got) != "ours" {
		t.Fatalf("joined dir = %q, %v; want current file", got, err)
	}
}

func TestStorePrepareMergeRebuildsCorruptEvidenceWithoutReplayingChanges(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "target", "child")
	if err := store.View("child").Write("optional.txt", []byte("from child")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "target", "join")
	if _, err := store.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}}); err != nil {
		t.Fatal(err)
	}
	joined := store.View("join")
	if err := joined.Delete("optional.txt"); err != nil {
		t.Fatal(err)
	}
	if err := joined.Write(MergeRuntimeDir+"/manifest.json", []byte("{")); err != nil {
		t.Fatal(err)
	}

	if _, err := store.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}}); err != nil {
		t.Fatalf("PrepareMerge after corrupt manifest: %v", err)
	}
	if _, err := joined.Read("optional.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("recovery replayed an excluded file: %v", err)
	}
	data, err := joined.Read(MergeRuntimeDir + "/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest MergeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("rebuilt manifest is invalid: %v", err)
	}
}

func TestPersistentStorePrepareMergeDoesNotReplayChangesAfterRestart(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	state := t.TempDir()
	first, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, first, "", "target")
	mustFork(t, first, "target", "child")
	if err := first.View("child").Write("optional.txt", []byte("from child")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, first, "target", "join")
	if _, err := first.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}}); err != nil {
		t.Fatal(err)
	}
	joined := first.View("join")
	if err := joined.Delete("optional.txt"); err != nil {
		t.Fatal(err)
	}
	if err := joined.Write(MergeRuntimeDir+"/manifest.json", []byte("{")); err != nil {
		t.Fatal(err)
	}

	second, err := NewPersistentStore(base, state)
	if err != nil {
		t.Fatal(err)
	}
	mustFork(t, second, "", "target")
	mustFork(t, second, "target", "child")
	mustFork(t, second, "target", "join")
	manifest, err := second.PrepareMerge("join", []MergeSource{{Name: "child-task", EnvID: "child"}})
	if err != nil {
		t.Fatalf("PrepareMerge after restart: %v", err)
	}
	if _, err := second.View("join").Read("optional.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("restart replayed an excluded file: %v", err)
	}
	if len(manifest.Changes) != 1 || manifest.Changes[0].Path != "optional.txt" {
		t.Fatalf("rebuilt manifest = %#v, want optional.txt evidence", manifest.Changes)
	}
}

func TestStoreMergeConflictsWhenGrandparentChangedAfterNestedFork(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	gp := store.View("gp")
	if err := gp.Write("shared.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "gp", "parent")
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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

func TestStoreMergeConflictsWhenMaskingFileChangedUnderChildWrite(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("dir", []byte("A")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := parent.Write("dir", []byte("B")); err != nil {
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
	if string(got) != "B" {
		t.Fatalf("parent dir = %q, want B", got)
	}
}

func TestStoreMergeConflictsWhenChildWritesUnderLiveFile(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("dir", []byte("A")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
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
	if string(got) != "A" {
		t.Fatalf("parent dir = %q, want A", got)
	}
}

func TestStoreMergeChildFileAtDirHidesTargetDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Write("dir/x.txt", []byte("from-parent")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := store.View("child").Write("dir", []byte("now-a-file")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got, err := parent.Read("dir")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "now-a-file" {
		t.Fatalf("parent dir = %q, want now-a-file", got)
	}
	if _, err := parent.Read("dir/x.txt"); err == nil {
		t.Fatal("file at dir left parent dir/x.txt visible")
	}
}

func TestStoreMergeRepeatedDirTombstoneHidesBaselineDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	parent := store.View("parent")
	if err := parent.Delete("dir"); err != nil {
		t.Fatal(err)
	}
	if err := parent.Write("dir/x.txt", []byte("recreated")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := store.View("child").Delete("dir"); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := parent.Read("dir/x.txt"); err == nil {
		t.Fatal("repeated dir tombstone left baseline dir/x.txt visible")
	}
}

func TestStoreMergeConflictsWhenChildHasFileAndDescendant(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "parent", "child")
	child := store.View("child")
	if err := child.Write("dir", []byte("file")); err != nil {
		t.Fatal(err)
	}
	if err := child.Write("dir/x.txt", []byte("child")); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "parent"); err == nil {
		t.Fatal("Merge succeeded, want conflict")
	}
}

func TestStoreMergeChildDeleteIntoGrandparentWithoutFileIsOK(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "gp", "parent")
	if err := store.View("parent").Write("temporary.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "parent", "child")
	if err := store.View("child").Delete("temporary.txt"); err != nil {
		t.Fatal(err)
	}

	if err := store.Merge("child", "gp"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := store.View("gp").Read("temporary.txt"); err == nil {
		t.Fatal("grandparent gained temporary.txt")
	}
}

func TestStoreMergeConflictsWhenGrandparentChangedDirDescendantAfterNestedFork(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	gp := store.View("gp")
	if err := gp.Write("dir/file.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	mustFork(t, store, "gp", "parent")
	mustFork(t, store, "parent", "child")
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
	mustFork(t, store, "parent", "child")
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

func TestStoreStatsExposeBoundedResourceInventory(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	mustFork(t, store, "parent", "child")
	if err := store.View("child").Write("created.txt", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := store.View("child").Delete("hello.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize("parent"); err != nil {
		t.Fatal(err)
	}
	defer store.Release("parent")

	got := store.Stats()
	if got.Environments != 2 || got.LiveDirs != 1 || got.OverlayFiles != 1 || got.Tombstones != 1 || got.OverlayBytes != 3 {
		t.Fatalf("stats = %#v", got)
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
	mustFork(t, store, "parent", "child")
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

func mustFork(t *testing.T, s *Store, parentID, childID string) {
	t.Helper()
	if err := s.Fork(parentID, childID); err != nil {
		t.Fatal(err)
	}
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
