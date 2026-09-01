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

// 继承是 fork 时刻的全量快照：spawn 后 from task 新增的节点不会自动流进 child，
// 回流只经 join 的 additive merge。
func TestStoresForkInheritsSnapshotNotLaterParentWrites(t *testing.T) {
	t.Parallel()

	memory := ctxgraph.NewStore()
	memory.Save("parent", ctxgraph.Graph{
		Nodes: []ctxgraph.Node{
			{ID: "task-info-task-1", Kind: ctxgraph.NodeKindDirective, Statement: "契约"},
			{ID: "n1", Kind: ctxgraph.NodeKindFact, Statement: "at-fork"},
		},
	})

	stores := Stores{Memory: memory}
	if err := stores.Fork("parent", "child"); err != nil {
		t.Fatal(err)
	}

	child := memory.Load("child")
	for _, want := range []string{"task-info-task-1", "n1"} {
		if !hasNodeID(child, want) {
			t.Fatalf("child missing inherited node %q: %#v", want, child.Nodes)
		}
	}

	parent := memory.Load("parent")
	parent.Nodes = append(parent.Nodes, ctxgraph.Node{
		ID:        "n2",
		Kind:      ctxgraph.NodeKindFact,
		Statement: "after-fork",
	})
	memory.Save("parent", parent)

	if got := memory.Load("child"); hasNodeID(got, "n2") {
		t.Fatalf("parent write after fork leaked into child: %#v", got.Nodes)
	}
}
