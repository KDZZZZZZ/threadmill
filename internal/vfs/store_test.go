package vfs

import (
	"os"
	"path/filepath"
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
