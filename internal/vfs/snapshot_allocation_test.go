package vfs_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestEnvironmentSnapshotsDoNotDuplicateFilePayloads(t *testing.T) {
	s := vfs.NewStore(t.TempDir())
	payload := make([]byte, 4<<20)
	payload[0] = 42
	if err := s.View("parent").Write("data", payload); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 64; i++ {
		if err := s.CreateEnvironment("parent", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	// Metadata for 64 one-file environments must not copy the 4 MiB payload 64 times.
	t.Logf("64 snapshots allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("64 snapshots allocated %d bytes; budget 32 MiB", allocated)
	}
	payload[0] = 99
	if err := s.View("parent").Write("data", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	first, err := s.View("0").Read("data")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 1
	if err := s.View("0").Write("data", []byte("child changed")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", "63"} {
		got, err := s.View(id).Read("data")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4<<20 || got[0] != 42 {
			t.Fatalf("snapshot %s changed", id)
		}
	}
	runtime.KeepAlive(s)
}
