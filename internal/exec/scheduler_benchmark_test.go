package exec

import (
	"context"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func BenchmarkSchedulerRunExternal(b *testing.B) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{name: "empty", command: "true"},
		{name: "output_cap", command: "head -c 262144 /dev/zero"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			scheduler := New(Config{
				Slots:           1,
				Timeout:         time.Minute,
				ExternalSandbox: true,
			})
			store := vfs.NewStore(b.TempDir())
			view := scheduler.View("benchmark", store)
			if _, err := view.Run(context.Background(), env.Cmd{Command: tc.command}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result, err := view.Run(context.Background(), env.Cmd{Command: tc.command})
				if err != nil || result.ExitCode != 0 {
					b.Fatalf("Run() = %#v, %v", result, err)
				}
			}
			b.StopTimer()
			if err := scheduler.Reap("benchmark"); err != nil {
				b.Fatal(err)
			}
		})
	}
}

func BenchmarkSchedulerRunExternalMountNamespace(b *testing.B) {
	if !probeExternalWorkspaceIsolation() {
		b.Skip("mount namespace isolation unavailable")
	}
	scheduler := New(Config{
		Slots:                      1,
		Timeout:                    time.Minute,
		ExternalSandbox:            true,
		ExternalWorkspaceIsolation: true,
	})
	store := vfs.NewStore(b.TempDir())
	view := scheduler.View("benchmark", store)
	if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		result, err := view.Run(context.Background(), env.Cmd{Command: "true"})
		if err != nil || result.ExitCode != 0 {
			b.Fatalf("Run() = %#v, %v", result, err)
		}
	}
	b.StopTimer()
	if err := scheduler.Reap("benchmark"); err != nil {
		b.Fatal(err)
	}
}

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
