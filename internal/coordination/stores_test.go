package coordination

import (
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestStoresForkCopiesMemoryAndFiles(t *testing.T) {
	t.Parallel()

	memory := ctxgraph.NewStore()
	memory.Save("parent", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{ID: "n1", Statement: "from-parent"}},
	})
	files := vfs.NewStore(t.TempDir())
	if err := files.View("parent").Write("note.txt", []byte("from-parent")); err != nil {
		t.Fatal(err)
	}

	stores := Stores{Memory: memory, Files: files}
	if err := stores.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}

	if got := memory.Load("child"); len(got.Nodes) != 1 || got.Nodes[0].Statement != "from-parent" {
		t.Fatalf("forked memory = %#v, want from-parent", got)
	}
	gotFile, err := files.View("child").Read("note.txt")
	if err != nil {
		t.Fatalf("child did not inherit parent file: %v", err)
	}
	if string(gotFile) != "from-parent" {
		t.Fatalf("child note.txt = %q, want from-parent", gotFile)
	}

	memory.Save("child", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{ID: "n1", Statement: "from-child"}},
	})
	if err := files.View("child").Write("note.txt", []byte("from-child")); err != nil {
		t.Fatal(err)
	}
	if err := files.View("child").Write("only-child.txt", []byte("secret")); err != nil {
		t.Fatal(err)
	}

	if memory.Load("parent").Nodes[0].Statement != "from-parent" {
		t.Fatal("child memory write leaked into parent")
	}
	stillParent, err := files.View("parent").Read("note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(stillParent) != "from-parent" {
		t.Fatal("child file write leaked into parent")
	}
	if _, err := files.View("parent").Read("only-child.txt"); err == nil {
		t.Fatal("parent saw child overlay file")
	}
}

func TestStoresMergeUnionsChildMemory(t *testing.T) {
	t.Parallel()

	memory := ctxgraph.NewStore()
	memory.Fork("parent", "child")
	memory.Save("child", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{ID: "c1", Statement: "from-child"}},
	})
	stores := Stores{Memory: memory, Files: vfs.NewStore(t.TempDir())}
	if err := stores.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge() = %v", err)
	}
	got := memory.Load("parent")
	if len(got.Nodes) != 1 || got.Nodes[0].Statement != "from-child" {
		t.Fatalf("merged parent = %#v, want from-child", got)
	}
	if err := (Stores{}).Merge("child", "parent"); err != nil {
		t.Fatalf("Merge() nil stores = %v, want nil", err)
	}
}

func TestStoresMergeUnionsChildFiles(t *testing.T) {
	t.Parallel()

	files := vfs.NewStore(t.TempDir())
	if err := files.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := files.View("child").Write("from-child.txt", []byte("from-child")); err != nil {
		t.Fatal(err)
	}
	stores := Stores{Memory: ctxgraph.NewStore(), Files: files}
	if err := stores.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge() = %v", err)
	}
	got, err := files.View("parent").Read("from-child.txt")
	if err != nil {
		t.Fatalf("merged parent missing child file: %v", err)
	}
	if string(got) != "from-child" {
		t.Fatalf("merged from-child.txt = %q, want from-child", got)
	}
}

func TestStoresMergeFileConflictDoesNotApplyMemory(t *testing.T) {
	t.Parallel()

	memory := ctxgraph.NewStore()
	memory.Fork("parent", "child")
	memory.Save("child", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{{ID: "c1", Statement: "from-child"}},
	})
	files := vfs.NewStore(t.TempDir())
	if err := files.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}
	if err := files.View("parent").Write("conflict.txt", []byte("ours")); err != nil {
		t.Fatal(err)
	}
	if err := files.View("child").Write("conflict.txt", []byte("theirs")); err != nil {
		t.Fatal(err)
	}

	err := (Stores{Memory: memory, Files: files}).Merge("child", "parent")
	if err == nil {
		t.Fatal("Merge succeeded, want file conflict")
	}
	if got := memory.Load("parent"); len(got.Nodes) != 0 {
		t.Fatalf("file conflict still merged memory: %#v", got.Nodes)
	}
}
