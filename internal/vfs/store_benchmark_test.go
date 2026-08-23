package vfs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func BenchmarkStoreStats1000Environments(b *testing.B) {
	store := NewStore(b.TempDir())
	for i := range 1000 {
		if err := store.View(fmt.Sprintf("env-%d", i)).Write("state.txt", []byte("state")); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = store.Stats()
	}
}

func BenchmarkMaterialize1000Files(b *testing.B) {
	benchmarkMaterializeFiles(b, 1000)
}

func BenchmarkMaterialize10000Files(b *testing.B) {
	benchmarkMaterializeFiles(b, 10000)
}

func BenchmarkMaterialize50000Files(b *testing.B) {
	benchmarkMaterializeFiles(b, 50000)
}

func BenchmarkAbsorbUnchanged32MiB(b *testing.B) {
	base := b.TempDir()
	payload := make([]byte, 1<<20)
	for i := range 32 {
		if err := os.WriteFile(filepath.Join(base, fmt.Sprintf("file-%02d.bin", i)), payload, 0o640); err != nil {
			b.Fatal(err)
		}
	}
	store := NewStore(base)
	if _, err := store.Materialize("env"); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(32 * int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := store.Absorb("env"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkMaterializeFiles(b *testing.B, count int) {
	base := b.TempDir()
	for i := range count {
		name := filepath.Join(base, fmt.Sprintf("pkg-%04d", i/100), fmt.Sprintf("file-%04d.txt", i))
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("fixture\n"), 0o640); err != nil {
			b.Fatal(err)
		}
	}
	store := NewStore(base)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		envID := fmt.Sprintf("env-%d", i)
		if _, err := store.Materialize(envID); err != nil {
			b.Fatal(err)
		}
		if err := store.Release(envID); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(count), "files/op")
}
