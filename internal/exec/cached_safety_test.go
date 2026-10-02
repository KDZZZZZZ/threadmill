package exec

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestFreshExecutionBypassesCacheLookupAndStore(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() { _ = sched.Reap("agent") })
	files := vfs.NewStore(baseRepo(t))
	command := env.Cmd{Command: "cat input.txt"}
	first, err := sched.View("agent", files).Run(t.Context(), command)
	if err != nil || first.ExitCode != 0 || cache.Stats().Stores != 1 {
		t.Fatalf("initial result = %+v, %v, cache=%+v", first, err, cache.Stats())
	}
	before := cache.Stats()
	started := sched.Stats().Started
	command.Fresh = true
	second, err := sched.View("agent", files).Run(t.Context(), command)
	if err != nil || second.Output != first.Output || second.CachedSegments != 0 {
		t.Fatalf("fresh result = %+v, %v", second, err)
	}
	after := cache.Stats()
	if sched.Stats().Started != started+1 || after.Lookups != before.Lookups || after.Stores != before.Stores {
		t.Fatalf("fresh must execute once and bypass cache: scheduler=%+v cache=%+v", sched.Stats(), after)
	}
}

func TestCacheRespectsEachEnvironmentsHomeInputs(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() { _ = sched.Reap("a"); _ = sched.Reap("b") })
	files := vfs.NewStore(baseRepo(t))
	for _, id := range []string{"a", "b"} {
		result, err := sched.View(id, files).Run(t.Context(), env.Cmd{Command: `printf first > "$HOME/config"`, Fresh: true})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("seed HOME %s = %+v, %v", id, result, err)
		}
	}
	read := env.Cmd{Command: `cat "$HOME/config"`}
	first, err := sched.View("a", files).Run(t.Context(), read)
	if err != nil || first.Output != "first" || cache.Stats().Stores != 1 {
		t.Fatalf("first HOME read = %+v, %v, cache=%+v", first, err, cache.Stats())
	}
	result, err := sched.View("b", files).Run(t.Context(), env.Cmd{Command: `printf second > "$HOME/config"`, Fresh: true})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("change HOME = %+v, %v", result, err)
	}
	started := sched.Stats().Started
	second, err := sched.View("b", files).Run(t.Context(), read)
	if err != nil || second.Output != "second" || sched.Stats().Started != started+1 {
		t.Fatalf("changed HOME reused an old result: %+v, %v, cache=%+v", second, err, cache.Stats())
	}
}

func TestCacheRetriesPytestAfterExternalPackageInstall(t *testing.T) {
	python := os.Getenv("THREADMILL_TEST_PYTEST_PYTHON")
	if python == "" {
		var err error
		python, err = osexec.LookPath("python3")
		if err != nil {
			t.Skip("python3 unavailable")
		}
	}
	if err := osexec.CommandContext(t.Context(), python, "-c", "import pytest").Run(); err != nil {
		t.Skip("pytest unavailable; set THREADMILL_TEST_PYTEST_PYTHON to a pytest interpreter")
	}
	site := t.TempDir()
	t.Setenv("PYTHONPATH", site)
	sched, cache := newCachedScheduler(t, Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() { _ = sched.Reap("python") })
	base := baseRepo(t)
	mustWrite(t, base, "test_package.py", "from threadmill_cache_probe import value\n\ndef test_installed():\n    assert value == 42\n")
	files := vfs.NewStore(base)
	command := env.Cmd{Command: "env PYTHONDONTWRITEBYTECODE=1 PYTEST_DISABLE_PLUGIN_AUTOLOAD=1 " + shellSingleQuote(python) + " -m pytest -q -p no:cacheprovider test_package.py"}
	first, err := sched.View("python", files).Run(t.Context(), command)
	if err != nil || first.ExitCode == 0 || !strings.Contains(first.Output, "ModuleNotFoundError") || cache.Stats().Stores != 1 {
		t.Fatalf("uninstalled pytest result = %+v, %v, cache=%+v", first, err, cache.Stats())
	}
	installed, err := sched.View("python", files).Run(t.Context(), env.Cmd{
		Command: `printf 'value = 42\n' > ` + shellSingleQuote(filepath.Join(site, "threadmill_cache_probe.py")),
		Fresh:   true,
	})
	if err != nil || installed.ExitCode != 0 {
		t.Fatalf("local package install = %+v, %v", installed, err)
	}
	started := sched.Stats().Started
	second, err := sched.View("python", files).Run(t.Context(), command)
	if err != nil || second.ExitCode != 0 || !strings.Contains(second.Output, "1 passed") || second.CachedSegments != 0 || sched.Stats().Started != started+1 {
		t.Fatalf("pytest reused the failure after package install = %+v, %v, cache=%+v", second, err, cache.Stats())
	}
}

func TestCacheMetadataAndRolesDescribeActualReplay(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() { _ = sched.Reap("a"); _ = sched.Reap("b") })
	files := vfs.NewStore(baseRepo(t))
	command := env.Cmd{Command: "cat input.txt", Role: "executor"}
	first, err := sched.View("a", files).Run(t.Context(), command)
	if err != nil || first.ExitCode != 0 || first.CachedSegments != 0 {
		t.Fatalf("initial execution = %+v, %v", first, err)
	}
	started := sched.Stats().Started
	command.Role = "verifier"
	second, err := sched.View("b", files).Run(t.Context(), command)
	if err != nil || second.Output != first.Output || second.CachedSegments != 1 || second.CacheSavedDuration <= 0 || second.CacheCreatorRole != "executor" || second.PeakRSSBytes != 0 {
		t.Fatalf("replay metadata = %+v, %v", second, err)
	}
	stats := cache.Stats()
	if sched.Stats().Started != started || stats.Roles["executor"].Executions != 1 || stats.Roles["verifier"].Replays != 1 || stats.CrossRoleHits != 1 || stats.ExecutedDuration <= 0 || stats.SavedDuration != second.CacheSavedDuration {
		t.Fatalf("execution/replay role accounting = %+v", stats)
	}
}

func TestCacheKeepsOutputLimitsInIdentity(t *testing.T) {
	firstScheduler, cache := newCachedScheduler(t, Config{Slots: 1, OutputCapKB: 1, ExternalSandbox: true})
	secondScheduler := New(Config{Slots: 1, OutputCapKB: 2, ExternalSandbox: true, Cache: cache})
	t.Cleanup(func() { _ = firstScheduler.Reap("small"); _ = secondScheduler.Reap("large") })
	files := vfs.NewStore(baseRepo(t))
	command := env.Cmd{Command: "printf '%04096d' 0"}
	first, err := firstScheduler.View("small", files).Run(t.Context(), command)
	if err != nil || first.ExitCode != 0 || len(first.Output) != 1024+len("\n[output truncated]\n") {
		t.Fatalf("small output cap = %+v, %v", first, err)
	}
	second, err := secondScheduler.View("large", files).Run(t.Context(), command)
	if err != nil || second.ExitCode != 0 || second.CachedSegments != 0 || len(second.Output) != 2048+len("\n[output truncated]\n") {
		t.Fatalf("different output cap reused a result = %+v, %v", second, err)
	}
	third, err := secondScheduler.View("large", files).Run(t.Context(), command)
	if err != nil || third.Output != second.Output || third.CachedSegments != 1 {
		t.Fatalf("same output cap failed to reuse its result = %+v, %v", third, err)
	}
}

func TestVerificationCountsOutputMismatchWithoutClaimingSavedTime(t *testing.T) {
	cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), VerifySampleRate: 1, CacheFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	sched := New(Config{Slots: 1, Cache: cache, ExternalSandbox: true})
	if !sched.Stats().DependencyTracing {
		t.Skip("syscall tracing unavailable")
	}
	t.Cleanup(func() { _ = sched.Reap("audit") })
	files := vfs.NewStore(baseRepo(t))
	command := env.Cmd{Command: `printf '%s' "$$"`, Role: "verifier"}
	first, err := sched.View("audit", files).Run(t.Context(), command)
	if err != nil || first.ExitCode != 0 || cache.Stats().Stores != 1 {
		t.Fatalf("initial process ID output = %+v, %v, cache=%+v", first, err, cache.Stats())
	}
	second, err := sched.View("audit", files).Run(t.Context(), command)
	if err != nil || second.ExitCode != 0 || second.Output == first.Output || second.CachedSegments != 0 || second.CacheSavedDuration != 0 {
		t.Fatalf("sampled re-execution = %+v, %v", second, err)
	}
	stats := cache.Stats()
	if stats.Verifications != 1 || stats.VerifyOutputMismatches != 1 || stats.VerifyMismatches != 1 || stats.VerifyExitMismatches != 0 || stats.VerifyWriteMismatches != 0 || stats.SavedDuration != 0 || stats.Replays != 0 || stats.VerificationDuration <= 0 {
		t.Fatalf("sampled output mismatch accounting = %+v", stats)
	}
}

func TestVerificationClassifiesExitAndWriteMismatches(t *testing.T) {
	for _, tt := range []struct {
		name, command        string
		exitCode             int
		wantExit, wantWrites uint64
	}{
		{name: "exit code", command: "printf actual", exitCode: 7, wantExit: 1},
		{name: "write set", command: "printf artifact > artifact.txt", wantWrites: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), VerifySampleRate: 1, CacheFailures: true})
			if err != nil {
				t.Fatal(err)
			}
			sched := New(Config{Slots: 1, Cache: cache, ExternalSandbox: true})
			if !sched.Stats().DependencyTracing {
				t.Skip("syscall tracing unavailable")
			}
			t.Cleanup(func() { _ = sched.Reap("audit") })
			files := vfs.NewStore(baseRepo(t))
			live, err := files.Materialize("audit")
			if err != nil {
				t.Fatal(err)
			}
			command := env.Cmd{Command: tt.command, Role: "verifier"}
			key := sched.cacheKey(command.Command, command.Role)
			// Seed a deliberately wrong candidate at the public cache seam.
			entry, err := cache.Store(live, key, cmdcache.Observation{}, cmdcache.Result{ExitCode: tt.exitCode, Duration: time.Second})
			if err != nil || entry == nil {
				t.Fatalf("seed incorrect cache result = %+v, %v", entry, err)
			}
			result, err := sched.View("audit", files).Run(t.Context(), command)
			if err != nil || result.ExitCode != 0 || result.CachedSegments != 0 {
				t.Fatalf("sampled execution = %+v, %v", result, err)
			}
			stats := cache.Stats()
			if stats.Verifications != 1 || stats.VerifyMismatches != 1 || stats.VerifyExitMismatches != tt.wantExit || stats.VerifyWriteMismatches != tt.wantWrites || stats.VerifyOutputMismatches != 0 || stats.SavedDuration != 0 {
				t.Fatalf("verification classification = %+v", stats)
			}
		})
	}
}

func TestBwrapRejectsDynamicVirtualContent(t *testing.T) {
	for _, command := range []string{"cat /proc/filesystems", "readlink /proc/self/exe"} {
		t.Run(command, func(t *testing.T) {
			sched, cache := newCachedScheduler(t, Config{Slots: 1})
			if sched.sandbox != sandboxBwrap {
				t.Skip("bwrap unavailable")
			}
			t.Cleanup(func() { _ = sched.Reap("virtual") })
			files := vfs.NewStore(baseRepo(t))
			for range 2 {
				result, err := sched.View("virtual", files).Run(t.Context(), env.Cmd{Command: command})
				if err != nil || result.ExitCode != 0 || result.Output == "" || result.CachedSegments != 0 {
					t.Fatalf("virtual content read = %+v, %v", result, err)
				}
			}
			if stats := cache.Stats(); stats.Stores != 0 || stats.RejectedReasons["unreadable_external"] != 2 || sched.Stats().Started != 2 {
				t.Fatalf("dynamic virtual content must execute: cache=%+v scheduler=%+v", stats, sched.Stats())
			}
		})
	}
}

func TestBwrapCachesFixedEOFDeviceRead(t *testing.T) {
	sched, cache := newCachedScheduler(t, Config{Slots: 1})
	if sched.sandbox != sandboxBwrap {
		t.Skip("bwrap unavailable")
	}
	t.Cleanup(func() { _ = sched.Reap("eof") })
	files := vfs.NewStore(baseRepo(t))
	command := env.Cmd{Command: "cat /dev/null"}
	first, err := sched.View("eof", files).Run(t.Context(), command)
	if err != nil || first.ExitCode != 0 || first.Output != "" || cache.Stats().Stores != 1 {
		t.Fatalf("fixed EOF read = %+v, %v, cache=%+v", first, err, cache.Stats())
	}
	second, err := sched.View("eof", files).Run(t.Context(), command)
	if err != nil || second.ExitCode != 0 || second.Output != "" || second.CachedSegments != 1 {
		t.Fatalf("fixed EOF replay = %+v, %v", second, err)
	}
}
