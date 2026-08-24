package vfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// applySafeJoin keeps the low-level conflict matrix focused on the safe join
// strategy without retaining the removed production auto-merge API.
func applySafeJoin(s *Store, candidateID, targetID string) error {
	result, err := s.ApplyJoin(candidateID, targetID, nil, false)
	if err != nil {
		return err
	}
	if len(result.Conflicts) > 0 {
		return fmt.Errorf("vfs: join conflict: %s", result.Conflicts[0])
	}
	return nil
}

func TestJoinChangesDoesNotModifyTarget(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	target := store.View("target")
	if err := target.Write("kept.txt", []byte("target")); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("target", "candidate"); err != nil {
		t.Fatal(err)
	}
	candidate := store.View("candidate")
	if err := candidate.Write("kept.txt", []byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Write("added.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}

	changes, err := store.JoinChanges("candidate")
	if err != nil {
		t.Fatal(err)
	}
	want := []JoinChange{
		{Path: "added.txt", Kind: JoinChangeAdded},
		{Path: "kept.txt", Kind: JoinChangeModified},
	}
	if !equalJoinChanges(changes, want) {
		t.Fatalf("JoinChanges() = %#v, want %#v", changes, want)
	}

	got, err := target.Read("kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "target" {
		t.Fatalf("target kept.txt = %q, want target", got)
	}
	if _, err := target.Read("added.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target added.txt error = %v, want fs.ErrNotExist", err)
	}
}

func TestApplyJoinAppliesOnlySelectedPaths(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	target := store.View("target")
	if err := target.Write("one.txt", []byte("base-one")); err != nil {
		t.Fatal(err)
	}
	if err := target.Write("two.txt", []byte("base-two")); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("target", "candidate"); err != nil {
		t.Fatal(err)
	}
	candidate := store.View("candidate")
	if err := candidate.Write("one.txt", []byte("candidate-one")); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Write("two.txt", []byte("candidate-two")); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyJoin("candidate", "target", []string{"one.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Applied, []string{"one.txt"}) || len(result.Conflicts) != 0 {
		t.Fatalf("ApplyJoin() = %#v, want one.txt applied", result)
	}
	assertFileBody(t, target, "one.txt", "candidate-one")
	assertFileBody(t, target, "two.txt", "base-two")
}

func TestApplyJoinConflictDoesNotPartiallyModifyTarget(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	target := store.View("target")
	if err := target.Write("conflict.txt", []byte("base-conflict")); err != nil {
		t.Fatal(err)
	}
	if err := target.Write("clean.txt", []byte("base-clean")); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("target", "candidate"); err != nil {
		t.Fatal(err)
	}
	candidate := store.View("candidate")
	if err := candidate.Write("conflict.txt", []byte("candidate")); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Write("clean.txt", []byte("candidate-clean")); err != nil {
		t.Fatal(err)
	}
	if err := target.Write("conflict.txt", []byte("target")); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyJoin("candidate", "target", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(result.Conflicts, []string{"conflict.txt"}) || len(result.Applied) != 0 {
		t.Fatalf("ApplyJoin() = %#v, want conflict and no applied paths", result)
	}
	assertFileBody(t, target, "conflict.txt", "target")
	assertFileBody(t, target, "clean.txt", "base-clean")
}

func TestApplyJoinPreservesDeletionAndExecutableBit(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	target := store.View("target")
	if err := target.Write("removed.txt", []byte("remove me")); err != nil {
		t.Fatal(err)
	}
	if err := store.Fork("target", "candidate"); err != nil {
		t.Fatal(err)
	}
	if err := store.View("candidate").Delete("removed.txt"); err != nil {
		t.Fatal(err)
	}
	live, err := store.Materialize("candidate")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "script.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Release("candidate"); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyJoin(
		"candidate",
		"target",
		[]string{"removed.txt", "script.sh"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 0 || !equalStrings(result.Applied, []string{"removed.txt", "script.sh"}) {
		t.Fatalf("ApplyJoin() = %#v", result)
	}
	if _, err := target.Read("removed.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed.txt error = %v, want fs.ErrNotExist", err)
	}
	targetLive, err := store.Materialize("target")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(targetLive, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("script.sh mode = %o, want executable", info.Mode().Perm())
	}
}

func equalJoinChanges(left, right []JoinChange) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func assertFileBody(t *testing.T, view *View, path, want string) {
	t.Helper()
	got, err := view.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
