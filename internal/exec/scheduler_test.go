package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestSchedulerRespectsSlotLimit(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	var current, max atomic.Int32
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		n := current.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		current.Add(-1)
		return env.ExecResult{}, nil
	}

	files := vfs.NewStore(t.TempDir())
	view := s.View("env-a", files)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	wg.Wait()
	if max.Load() > 1 {
		t.Fatalf("concurrent runs = %d, want at most 1", max.Load())
	}
}

func TestSchedulerStatsExposeQueueSaturationAndCompletion(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		once.Do(func() { close(entered) })
		<-release
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()
	<-entered
	go func() {
		defer wg.Done()
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()

	deadline := time.Now().Add(time.Second)
	for {
		stats := s.Stats()
		if stats.Active == 1 && stats.Queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats before release = %#v", stats)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	got := s.Stats()
	if got.Capacity != 1 || got.Requests != 2 || got.Started != 2 || got.Completed != 2 {
		t.Fatalf("stats = %#v", got)
	}
	if got.Active != 0 || got.Queued != 0 || got.PeakActive != 1 || got.PeakQueued < 1 {
		t.Fatalf("saturation stats = %#v", got)
	}
	if got.WaitDuration <= 0 || got.RunDuration <= 0 {
		t.Fatalf("durations = %#v", got)
	}
}

func TestSchedulerStatsClassifyCanceledWait(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	entered := make(chan struct{})
	release := make(chan struct{})
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		close(entered)
		<-release
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = view.Run(context.Background(), env.Cmd{Command: "true"})
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.Run(ctx, env.Cmd{Command: "true"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	close(release)
	<-done
	got := s.Stats()
	if got.Requests != 2 || got.Completed != 2 || got.Canceled != 1 || got.Errors != 1 {
		t.Fatalf("stats = %#v", got)
	}
}

func TestSchedulerStatsDoNotCountImmediateSlotAsQueued(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	s.run = func(context.Context, string, env.Cmd) (env.ExecResult, error) {
		return env.ExecResult{}, nil
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	if _, err := view.Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	got := s.Stats()
	if got.Queued != 0 || got.PeakQueued != 0 || got.WaitDuration != 0 {
		t.Fatalf("queue stats = %#v", got)
	}
}

func TestSchedulerLeavesLiveWritesForRelease(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	var live string
	s.run = func(_ context.Context, dir string, _ env.Cmd) (env.ExecResult, error) {
		live = dir
		if err := os.WriteFile(filepath.Join(dir, "from-run.txt"), []byte("x"), 0o640); err != nil {
			return env.ExecResult{}, err
		}
		return env.ExecResult{}, nil
	}
	files := vfs.NewStore(t.TempDir())
	if err := files.View("env-a").Write("overlay.txt", []byte("overlay")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live dir after Run: %v", err)
	}
	if err := files.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := files.View("env-a").Read("from-run.txt")
	if err != nil {
		t.Fatalf("Release dropped live write: %v", err)
	}
	if string(got) != "x" {
		t.Fatalf("from-run.txt = %q, want x", got)
	}
}

func TestSchedulerUnavailableSandbox(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	s.sandbox = sandboxNone
	s.run = nil
	files := vfs.NewStore(t.TempDir())
	_, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"})
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Run error = %v, want ErrSandboxUnavailable", err)
	}
	if err != nil && !strings.Contains(err.Error(), "SANDBOX_UNAVAILABLE") {
		t.Fatalf("error %v missing SANDBOX_UNAVAILABLE", err)
	}
}

func TestSchedulerStatsReportIsolationBoundary(t *testing.T) {
	t.Parallel()

	got := New(Config{Slots: 1, ExternalSandbox: true}).Stats()
	if got.SandboxBackend != "external" || got.NetworkIsolation != "external" {
		t.Fatalf("Stats() = %#v", got)
	}
}

func TestSchedulerBwrapSharesNetwork(t *testing.T) {
	bin := t.TempDir()
	bwrap := filepath.Join(bin, "bwrap")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
	case "$1" in
		--unshare-net) exit 64 ;;
		--) shift; exec "$@" ;;
	esac
	shift
done
exit 65
`
	if err := os.WriteFile(bwrap, []byte(script), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("https_proxy", "http://127.0.0.1:43123")

	files := vfs.NewStore(t.TempDir())
	s := New(Config{Slots: 1})
	t.Cleanup(func() {
		if err := s.Reap("env-a"); err != nil {
			t.Error(err)
		}
	})
	stats := s.Stats()
	if stats.SandboxBackend != "bwrap" || stats.NetworkIsolation != "shared" {
		t.Fatalf("Stats() = %#v, want bwrap with shared network", stats)
	}
	result, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `test "$https_proxy" = "http://127.0.0.1:43123"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, want inherited network environment", result)
	}
}

func TestSchedulerRunsInsideExternalSandbox(t *testing.T) {
	t.Parallel()

	files := vfs.NewStore(t.TempDir())
	if err := files.View("env-a").Write("input.txt", []byte("inside")); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Slots: 1, ExternalSandbox: true})
	result, err := s.View("env-a", files).Run(
		context.Background(),
		env.Cmd{Command: `test "$HOME" != "$PWD" && cat input.txt > output.txt`},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d, output = %q", result.ExitCode, result.Output)
	}
	if got := s.Stats().TrackedProcessGroups; got != 0 {
		t.Fatalf("tracked process groups after command exit = %d, want 0", got)
	}
	if err := s.Reap("env-a"); err != nil {
		t.Fatal(err)
	}
	if err := files.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := files.View("env-a").Read("output.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "inside" {
		t.Fatalf("output.txt = %q, want inside", got)
	}
}

func TestExternalSandboxUsesPerEnvironmentTemp(t *testing.T) {
	t.Parallel()

	marker := filepath.Base(t.TempDir())
	defer os.Remove(filepath.Join(os.TempDir(), marker))
	files := vfs.NewStore(t.TempDir())
	s := New(Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() {
		if err := errors.Join(s.Reap("env-a"), s.Reap("env-b")); err != nil {
			t.Error(err)
		}
	})
	first, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `touch "$TMPDIR/` + marker + `"`,
	})
	if err != nil || first.ExitCode != 0 {
		t.Fatalf("first Run() = %#v, %v", first, err)
	}
	reused, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `test -e "$TMPDIR/` + marker + `"`,
	})
	if err != nil || reused.ExitCode != 0 {
		t.Fatalf("second Run() in same environment = %#v, %v; temp state was not reused", reused, err)
	}
	if err := files.Fork("env-a", "env-b"); err != nil {
		t.Fatal(err)
	}
	second, err := s.View("env-b", files).Run(context.Background(), env.Cmd{
		Command: `test ! -e "$TMPDIR/` + marker + `"`,
	})
	if err != nil || second.ExitCode != 0 {
		t.Fatalf("second Run() = %#v, %v; temp state crossed environments", second, err)
	}
}

func TestExternalSandboxReusesBuildCacheOnlyWithinEnvironment(t *testing.T) {
	t.Parallel()

	files := vfs.NewStore(t.TempDir())
	s := New(Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() {
		if err := errors.Join(s.Reap("env-a"), s.Reap("env-b")); err != nil {
			t.Error(err)
		}
	})
	writeMarker := `cache=$(go env GOCACHE) && mkdir -p "$cache" && touch "$cache/threadmill-marker"`
	if result, err := s.View("env-a", files).Run(
		context.Background(), env.Cmd{Command: writeMarker},
	); err != nil || result.ExitCode != 0 {
		t.Fatalf("warm cache = %#v, %v", result, err)
	}
	if result, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `test -e "$(go env GOCACHE)/threadmill-marker"`,
	}); err != nil || result.ExitCode != 0 {
		t.Fatalf("reuse cache = %#v, %v", result, err)
	}
	if err := files.Fork("env-a", "env-b"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.View("env-b", files).Run(context.Background(), env.Cmd{
		Command: `test ! -e "$(go env GOCACHE)/threadmill-marker"`,
	}); err != nil || result.ExitCode != 0 {
		t.Fatalf("sibling cache isolation = %#v, %v", result, err)
	}
}

func TestExternalSandboxForwardsOnlyNetworkEnvironment(t *testing.T) {
	networkEnvironment := map[string]string{
		"all_proxy":           "socks5://127.0.0.1:43001",
		"http_proxy":          "http://127.0.0.1:43002",
		"https_proxy":         "http://127.0.0.1:43003",
		"no_proxy":            "localhost,127.0.0.1",
		"ALL_PROXY":           "socks5://127.0.0.1:43004",
		"HTTP_PROXY":          "http://127.0.0.1:43005",
		"HTTPS_PROXY":         "http://127.0.0.1:43006",
		"NO_PROXY":            "example.test",
		"CURL_CA_BUNDLE":      "/operator/curl-ca.pem",
		"GIT_SSL_CAINFO":      "/operator/git-ca.pem",
		"NODE_EXTRA_CA_CERTS": "/operator/node-ca.pem",
		"REQUESTS_CA_BUNDLE":  "/operator/requests-ca.pem",
		"SSL_CERT_DIR":        "/operator/certs",
		"SSL_CERT_FILE":       "/operator/ca.pem",
	}
	checks := make([]string, 0, len(networkEnvironment)+1)
	for name, value := range networkEnvironment {
		t.Setenv(name, value)
		checks = append(checks, `test "$`+name+`" = "`+value+`"`)
	}
	t.Setenv("THREADMILL_TEST_SECRET", "must-not-cross")
	checks = append(checks, `test -z "$THREADMILL_TEST_SECRET"`)

	files := vfs.NewStore(t.TempDir())
	s := New(Config{Slots: 1, ExternalSandbox: true})
	t.Cleanup(func() {
		if err := s.Reap("env-a"); err != nil {
			t.Error(err)
		}
	})
	result, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: strings.Join(checks, " && "),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, want network environment without arbitrary host variables", result)
	}
}

func TestSchedulerReapKillsTrackedProcessGroup(t *testing.T) {
	t.Parallel()

	cmd := osexec.Command("bash", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	s := New(Config{Slots: 1})
	s.track("env-a", pgid)
	if err := s.Reap("env-a"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("sleep still running after Reap")
	}
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("process group still alive after Reap")
	}
}

func TestSchedulerKeepsLiveBackgroundProcessGroupForReap(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1, ExternalSandbox: true})
	files := vfs.NewStore(t.TempDir())
	result, err := s.View("env-a", files).Run(
		context.Background(),
		env.Cmd{Command: "sleep 30 &"},
	)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if got := s.Stats().TrackedProcessGroups; got != 1 {
		t.Fatalf("tracked process groups with background child = %d, want 1", got)
	}
	if err := s.Reap("env-a"); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats().TrackedProcessGroups; got != 0 {
		t.Fatalf("tracked process groups after Reap = %d, want 0", got)
	}
}

func TestSchedulerWithoutBwrapDoesNotRun(t *testing.T) {
	t.Parallel()
	if probeBwrap() {
		t.Skip("bwrap available")
	}

	s := New(Config{Slots: 1})
	files := vfs.NewStore(t.TempDir())
	_, err := s.View("env-a", files).Run(context.Background(), env.Cmd{Command: "true"})
	if !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("Run error = %v, want ErrSandboxUnavailable", err)
	}
}
