package exec

import (
	"context"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func BenchmarkSchedulerRunParallel(b *testing.B) {
	scheduler := New(Config{Slots: 4})
	scheduler.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		return env.ExecResult{}, nil
	}
	view := scheduler.View("benchmark", vfs.NewStore(b.TempDir()))
	if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
				b.Error(err)
			}
		}
	})
}
