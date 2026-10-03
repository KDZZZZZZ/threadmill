//go:build integration

package exec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func astExecution(t *testing.T) (*exec.Scheduler, *cmdcache.Cache, *vfs.Store) {
	t.Helper()
	cache, err := cmdcache.New(cmdcache.Config{Dir: t.TempDir(), CacheFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := exec.New(exec.Config{
		Slots: 1, Timeout: 10 * time.Second, Cache: cache, ExternalSandbox: true,
	})
	if !scheduler.Stats().DependencyTracing {
		t.Skipf("tracing unavailable: %s", scheduler.Stats().DependencyTracingReason)
	}
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "input.txt"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := vfs.NewStore(base)
	t.Cleanup(func() {
		for _, id := range []string{"ast-a", "ast-b"} {
			if err := scheduler.Reap(id); err != nil {
				t.Error(err)
				continue
			}
			if err := files.Discard(id); err != nil {
				t.Error(err)
			}
		}
	})
	return scheduler, cache, files
}

func TestCachedExecutionASTKeyReusesStaticSpelling(t *testing.T) {
	scheduler, cache, files := astExecution(t)
	first, err := scheduler.View("ast-a", files).Run(t.Context(), env.Cmd{Command: "  cat\t'input.txt'  "})
	if err != nil || first.ExitCode != 0 || first.Output != "payload\n" || cache.Stats().Stores != 1 {
		t.Fatalf(
			"initial Run = %+v, %v, cache=%+v",
			first,
			err,
			cache.Stats(),
		)
	}
	started := scheduler.Stats().Started
	second, err := scheduler.View("ast-b", files).Run(t.Context(), env.Cmd{Command: "cat \"input.txt\""})
	if err != nil || second.ExitCode != 0 || second.Output != "payload\n" || second.CachedSegments != 1 {
		t.Fatalf("equivalent spelling did not replay = %+v, %v", second, err)
	}
	if scheduler.Stats().Started != started {
		t.Fatal("equivalent spelling consumed an execution slot")
	}
}

func TestCachedExecutionASTKeyRunsOriginalBashSource(t *testing.T) {
	scheduler, _, files := astExecution(t)
	for _, command := range []string{
		`printf '%s' "$BASH_EXECUTION_STRING"`,
		`  printf   '%s' "$BASH_EXECUTION_STRING"  `,
	} {
		result, err := scheduler.View("ast-a", files).Run(t.Context(), env.Cmd{Command: command})
		if err != nil || result.ExitCode != 0 || result.Output != command || result.CachedSegments != 0 {
			t.Fatalf(
				"Run rewrote or reused observable source %q: %+v, %v",
				command,
				result,
				err,
			)
		}
	}
}

func TestCachedExecutionASTKeyPreservesLineNumberOutput(t *testing.T) {
	scheduler, _, files := astExecution(t)
	for _, tt := range []struct{ name, command, output string }{
		{name: "line two", command: "\nprintf '%s' \"$LINENO\"", output: "2"},
		{name: "line three", command: "\n\nprintf '%s' \"$LINENO\"", output: "3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := scheduler.View("ast-a", files).Run(t.Context(), env.Cmd{Command: tt.command})
			if err != nil || result.ExitCode != 0 || result.Output != tt.output || result.CachedSegments != 0 {
				t.Fatalf("Run merged different line numbers: %+v, %v", result, err)
			}
		})
	}
}

func TestCachedExecutionASTKeyReadsSourceByStaticVariableName(t *testing.T) {
	scheduler, cache, files := astExecution(t)
	for _, command := range []string{
		`'declare' -p BASH_EXECUTION_STRING`,
		`'declare'  -p BASH_EXECUTION_STRING`,
		`'typeset' -p BASH_EXECUTION_STRING`,
		`'typeset'  -p BASH_EXECUTION_STRING`,
	} {
		stores := cache.Stats().Stores
		result, err := scheduler.View("ast-a", files).Run(t.Context(), env.Cmd{Command: command})
		if err != nil || result.ExitCode != 0 || !strings.Contains(result.Output, command) ||
			result.CachedSegments != 0 || cache.Stats().Stores != stores+1 {
			t.Fatalf(
				"Run reused or rewrote source read by variable name %q: %+v, %v, cache=%+v",
				command,
				result,
				err,
				cache.Stats(),
			)
		}
	}
}
