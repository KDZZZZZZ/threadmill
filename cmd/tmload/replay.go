package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	"github.com/KDZZZZZZ/threadmill/internal/env"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

type replayOptions struct {
	Repo, Workdir, LiveRoot, Sandbox string
	Slots                            int
	Cache, Tracing, Retain           bool
	Verify                           float64
}

type operationResult struct {
	Agent         string `json:"agent"`
	Index         int    `json:"index"`
	Op            string `json:"op"`
	DurationNS    int64  `json:"duration_ns"`
	ExitCode      int    `json:"exit_code,omitempty"`
	OutputSHA256  string `json:"output_sha256,omitempty"`
	Output        string `json:"output,omitempty"`
	Cached        bool   `json:"cached,omitempty"`
	ExpectedCache *bool  `json:"expected_cache,omitempty"`
	Error         string `json:"error,omitempty"`
}

type replayStat struct {
	Count int   `json:"count"`
	P50NS int64 `json:"p50_ns"`
	P95NS int64 `json:"p95_ns"`
}

type replayReport struct {
	Version               int                   `json:"version"`
	Backend               string                `json:"backend"`
	FixtureCommit         string                `json:"fixture_commit"`
	TraceSHA256           string                `json:"trace_sha256"`
	Agents                int                   `json:"agents"`
	Serial                bool                  `json:"serial"`
	WallNS                int64                 `json:"wall_ns"`
	CommandsPS            float64               `json:"commands_per_second"`
	Errors                int                   `json:"errors"`
	CacheOracleMismatches int                   `json:"cache_oracle_mismatches"`
	Operations            []operationResult     `json:"operations"`
	Latency               map[string]replayStat `json:"latency"`
	Execution             tmexec.Stats          `json:"execution"`
	VFS                   vfs.Stats             `json:"vfs"`
	PeakRSS               string                `json:"rss_peak"`
	SetupNS               int64                 `json:"setup_ns"`
}

func gitOutput(repo string, args ...string) (string, error) {
	argv := append([]string{"-c", "gc.auto=0", "-C", repo}, args...)
	out, err := osexec.Command("git", argv...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func verifyFixture(repo, commit string) error {
	if !commitID.MatchString(commit) {
		return fmt.Errorf("replay requires a pinned fixture commit; export with -repo")
	}
	head, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != commit {
		return fmt.Errorf("fixture HEAD %s does not match trace commit %s", head, commit)
	}
	status, err := gitOutput(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("fixture must be clean: %s", status)
	}
	return nil
}

// git archive supplies exactly the committed worktree contents, excluding .git.
// Both backends check HEAD before replay; uncommitted fixture edits cannot leak in.
func archiveFixture(repo, commit, dest string) error {
	if err := os.MkdirAll(dest, 0o750); err != nil {
		return err
	}
	archive := osexec.Command("git", "-C", repo, "archive", "--format=tar", commit)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	extract := osexec.Command("tar", "-xf", "-", "-C", dest)
	extract.Stdin = pipe
	if err := archive.Start(); err != nil {
		return err
	}
	extractErr := extract.Run()
	if extractErr != nil {
		_ = archive.Process.Kill()
	}
	return errors.Join(extractErr, archive.Wait())
}

func runTrace(ctx context.Context, trace loadTrace, cfg replayOptions) (report replayReport, retErr error) {
	if err := validateTrace(trace); err != nil {
		return report, err
	}
	if cfg.Sandbox != "external" && cfg.Sandbox != "bwrap" {
		return report, fmt.Errorf("sandbox must be external or bwrap")
	}
	if cfg.Cache && !cfg.Tracing {
		return report, fmt.Errorf("cache replay requires dependency tracing")
	}
	if cfg.Workdir == "" || cfg.Repo == "" {
		return report, fmt.Errorf("trace replay requires -repo and -workdir")
	}
	if err := verifyFixture(cfg.Repo, trace.Fixture.Commit); err != nil {
		return report, err
	}
	contents, err := os.ReadDir(cfg.Workdir)
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	if len(contents) != 0 {
		return report, fmt.Errorf("trace replay requires an empty workdir: %s", cfg.Workdir)
	}
	setup := time.Now()
	source := filepath.Join(cfg.Workdir, "source")
	if err := archiveFixture(cfg.Repo, trace.Fixture.Commit, source); err != nil {
		return report, err
	}
	if cfg.LiveRoot == "" {
		cfg.LiveRoot = filepath.Join(cfg.Workdir, "live")
	}
	store, err := vfs.NewPersistentStore(source, cfg.LiveRoot)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, store.Close()) }()
	var cache *cmdcache.Cache
	if cfg.Cache {
		cache, err = cmdcache.New(cmdcache.Config{
			Dir: filepath.Join(cfg.Workdir, "cache"), CacheFailures: true, VerifySampleRate: cfg.Verify,
		})
		if err != nil {
			return report, err
		}
	}
	sched := tmexec.New(tmexec.Config{
		Slots: cfg.Slots, Timeout: 120 * time.Second, ExternalSandbox: cfg.Sandbox == "external",
		HeavyThreshold: 24 * time.Hour, Cache: cache, DisableTrace: !cfg.Tracing, DependencyTracing: cfg.Tracing,
	})
	if cfg.Tracing && !sched.Stats().DependencyTracing {
		return report, fmt.Errorf("requested dependency tracing is unavailable")
	}
	report = replayReport{Version: 1, Backend: "threadmill-" + cfg.Sandbox,
		FixtureCommit: trace.Fixture.Commit, Agents: len(trace.Agents), Serial: trace.Serial,
		Latency: make(map[string]replayStat), SetupNS: time.Since(setup).Nanoseconds()}
	raw, _ := json.Marshal(trace)
	digest := sha256.Sum256(raw)
	report.TraceSHA256 = hex.EncodeToString(digest[:])
	m := newMetrics()
	var mu sync.Mutex
	record := func(row operationResult) {
		mu.Lock()
		report.Operations = append(report.Operations, row)
		if row.Error != "" {
			report.Errors++
		}
		if cfg.Cache && row.ExpectedCache != nil && *row.ExpectedCache != row.Cached {
			report.CacheOracleMismatches++
		}
		mu.Unlock()
		m.observe(row.Op, time.Duration(row.DurationNS))
	}
	work := func(agent traceAgent) {
		started := time.Now()
		err := store.CreateEnvironment("", agent.ID)
		fork := operationResult{Agent: agent.ID, Index: -1, Op: "fork", DurationNS: time.Since(started).Nanoseconds()}
		if err != nil {
			fork.Error = err.Error()
		}
		record(fork)
		defer func() {
			started := time.Now()
			err := errors.Join(sched.Reap(agent.ID), store.Release(agent.ID), store.Discard(agent.ID))
			row := operationResult{Agent: agent.ID, Index: len(agent.Operations) + 1, Op: "release", DurationNS: time.Since(started).Nanoseconds()}
			if err != nil {
				row.Error = err.Error()
			}
			record(row)
		}()
		if err != nil {
			return
		}
		view := store.View(agent.ID)
		x := sched.View(agent.ID, store)
		for index, op := range agent.Operations {
			if ctx.Err() != nil {
				break
			}
			started := time.Now()
			row := operationResult{Agent: agent.ID, Index: index, Op: op.Op, ExpectedCache: op.ExpectedCache}
			var err error
			switch op.Op {
			case "think":
				select {
				case <-time.After(time.Duration(op.DurationNS)):
				case <-ctx.Done():
					err = ctx.Err()
				}
			case "read":
				_, err = view.Read(op.Path)
			case "list":
				_, err = view.List(op.Path)
			case "write":
				err = view.Write(op.Path, []byte(op.Content))
			case "bash":
				var result env.ExecResult
				result, err = x.Run(ctx, env.Cmd{Command: op.Command, Timeout: 120 * time.Second})
				row.ExitCode = result.ExitCode
				row.Cached = result.CachedSegments > 0
				row.Output = result.Output
				digest := sha256.Sum256([]byte(result.Output))
				row.OutputSHA256 = hex.EncodeToString(digest[:])
				if err == nil && result.ExitCode != op.ExpectedExit {
					err = fmt.Errorf("exit code %d, want %d", result.ExitCode, op.ExpectedExit)
				}
			}
			row.DurationNS = time.Since(started).Nanoseconds()
			if err != nil {
				row.Error = err.Error()
			}
			record(row)
		}
		started = time.Now()
		err = store.Absorb(agent.ID)
		if err == nil && cfg.Retain {
			err = store.Archive(agent.ID, "checkpoint-"+agent.ID)
		}
		row := operationResult{Agent: agent.ID, Index: len(agent.Operations), Op: "collect", DurationNS: time.Since(started).Nanoseconds()}
		if err != nil {
			row.Error = err.Error()
		}
		record(row)
	}
	started := time.Now()
	if trace.Serial {
		for _, agent := range trace.Agents {
			work(agent)
		}
	} else {
		var wg sync.WaitGroup
		for _, agent := range trace.Agents {
			wg.Add(1)
			go func(agent traceAgent) { defer wg.Done(); work(agent) }(agent)
		}
		wg.Wait()
	}
	report.WallNS = time.Since(started).Nanoseconds()
	for op, stat := range m.summary() {
		report.Latency[op] = replayStat{Count: stat.n, P50NS: stat.p50.Nanoseconds(), P95NS: stat.p95.Nanoseconds()}
	}
	report.CommandsPS = float64(report.Latency["bash"].Count) / time.Duration(report.WallNS).Seconds()
	report.Execution, report.VFS, report.PeakRSS = sched.Stats(), store.Stats(), peakRSS()
	sort.Slice(report.Operations, func(i, j int) bool {
		a, b := report.Operations[i], report.Operations[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Index < b.Index
	})
	if report.Errors > 0 {
		retErr = fmt.Errorf("trace replay had %d operation errors", report.Errors)
	}
	return report, errors.Join(retErr, ctx.Err())
}
