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
	stores.Fork("parent", "child")

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

func TestStoresMergeIsNoOpUntilJ(t *testing.T) {
	t.Parallel()

	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}
	if err := stores.Merge("child", "parent"); err != nil {
		t.Fatalf("Merge() = %v, want nil until store Merge lands", err)
	}
	if err := (Stores{}).Merge("child", "parent"); err != nil {
		t.Fatalf("Merge() nil stores = %v, want nil", err)
	}
}
