package exec

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestSchedulerRejectsUnusableDependencyTracer(t *testing.T) {
	if os.Getenv("THREADMILL_TRACE_PROBE_TEST") == "child" {
		cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		s := New(Config{Slots: 1, Cache: cache, ExternalSandbox: true})
		if s.Stats().DependencyTracing {
			t.Fatal("unusable tracer was reported active")
		}
		if pidFile := os.Getenv("THREADMILL_TRACE_PROBE_PID_FILE"); pidFile != "" {
			data, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			stat, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(data)), "stat"))
			if err == nil {
				end := strings.LastIndex(string(stat), ") ")
				if end >= 0 && !strings.HasPrefix(string(stat)[end+2:], "Z ") {
					t.Fatalf("startup capability probe left a live descendant: %s", stat)
				}
			}
		}
		return
	}
	for _, tt := range []struct {
		name, script, reason string
		background           bool
	}{
		{name: "missing", reason: "not_found"},
		{name: "old version", script: "#!/bin/sh\nprintf 'strace -- version 5.2\\n'\n", reason: "unsupported_version"},
		{name: "blocked ptrace", script: "#!/bin/sh\nif [ \"$1\" = -V ]; then printf 'strace -- version 6.8\\n'; exit 0; fi\nexit 1\n", reason: "probe_failed"},
		{name: "live descendant", script: "#!/bin/sh\nif [ \"$1\" = -V ]; then printf 'strace -- version 6.8\\n'; exit 0; fi\n/bin/sleep 30 &\nprintf '%s' \"$!\" > \"$THREADMILL_TRACE_PROBE_PID_FILE\"\nexit 0\n", reason: "probe_failed", background: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "strace"), []byte(tt.script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			program, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := osexec.Command(program, "-test.run=^TestSchedulerRejectsUnusableDependencyTracer$")
			cmd.Env = append(os.Environ(), "PATH="+dir, "THREADMILL_TRACE_PROBE_TEST=child")
			if tt.background {
				pidFile := filepath.Join(dir, "pid")
				cmd.Env = append(cmd.Env, "THREADMILL_TRACE_PROBE_PID_FILE="+pidFile)
				t.Cleanup(func() {
					if data, err := os.ReadFile(pidFile); err == nil {
						if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
							_ = syscall.Kill(pid, syscall.SIGKILL)
						}
					}
				})
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("probe subprocess: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), "dependency tracing unavailable") || !strings.Contains(string(out), tt.reason) {
				t.Fatalf("missing tracing warning/reason %q: %s", tt.reason, out)
			}
		})
	}
}

func TestDependencyTracingProbeAndCacheOffInstrumentation(t *testing.T) {
	if tracerPath() == "" {
		t.Skip("syscall tracing unavailable")
	}
	s := New(Config{Slots: 1, ExternalSandbox: true})
	available, reason := s.ProbeDependencyTracing(t.Context())
	if !available || reason != "" {
		t.Fatalf("dependency tracing capability = %t, %q", available, reason)
	}
	if stats := s.Stats(); stats.DependencyTracing || stats.RuntimeDirs != 0 || stats.Started != 0 || stats.TrackedProcessGroups != 0 {
		t.Fatalf("capability probe changed execution state: %+v", stats)
	}
	s = New(Config{Slots: 1, ExternalSandbox: true, DependencyTracing: true})
	t.Cleanup(func() { _ = s.Reap("agent") })
	if !s.Stats().DependencyTracing {
		t.Fatal("explicit cache-off tracing was not enabled")
	}
	result, err := s.View("agent", vfs.NewStore(t.TempDir())).Run(t.Context(), env.Cmd{Command: "printf measured"})
	if err != nil || result.ExitCode != 0 || result.Output != "measured" || result.CachedSegments != 0 {
		t.Fatalf("cache-off traced execution = %+v, %v", result, err)
	}
}
