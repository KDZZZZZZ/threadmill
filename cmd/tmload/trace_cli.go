package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
)

func traceCLI(output, input, reportPath string, generation traceGeneration, duty float64, cfg replayOptions) error {
	if output != "" && input != "" {
		return fmt.Errorf("trace-out and trace-in are mutually exclusive")
	}
	if cfg.Verify < 0 || cfg.Verify > 1 {
		return fmt.Errorf("cache-verify-sample-rate must be between zero and one")
	}
	if output != "" {
		if generation.Think < 0 {
			if duty <= 0 || duty >= 1 {
				return fmt.Errorf("command-duty must be between zero and one")
			}
			generation.Think = targetThinkMultiplier(duty, generation.Scale)
		}
		if cfg.Repo == "" {
			if cfg.Workdir == "" {
				return fmt.Errorf("trace export needs -repo or -workdir to retain its committed fixture")
			}
			var err error
			cfg.Repo, err = makeFixtureRepo(cfg.Workdir, generation.Files, generation.FileBytes/1024)
			if err != nil {
				return err
			}
			if err := commitFixture(cfg.Repo); err != nil {
				return err
			}
		} else {
			paths, err := gitOutput(cfg.Repo, "ls-files", "-z")
			if err != nil {
				return err
			}
			generation.Paths = strings.Split(strings.TrimSuffix(paths, "\x00"), "\x00")
			if len(generation.Paths) == 0 || generation.Paths[0] == "" {
				return fmt.Errorf("fixture contains no tracked files")
			}
		}
		commit, err := gitOutput(cfg.Repo, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if err := verifyFixture(cfg.Repo, commit); err != nil {
			return err
		}
		generation.Commit = commit
		trace, err := generateTrace(generation)
		if err != nil {
			return err
		}
		return writeJSON(output, trace)
	}
	raw, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	trace, err := decodeTrace(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	report, replayErr := runTrace(context.Background(), trace, cfg)
	digest := sha256.Sum256(raw)
	report.TraceSHA256 = hex.EncodeToString(digest[:])
	if report.Version != 0 {
		if reportPath == "" {
			err = json.NewEncoder(os.Stdout).Encode(report)
		} else {
			err = writeJSON(reportPath, report)
		}
	}
	return errors.Join(replayErr, err)
}

func writeJSON(dest string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, append(data, '\n'), 0o640)
}

func commitFixture(repo string) error {
	for _, args := range [][]string{{"init", "--quiet"}, {"config", "gc.auto", "0"}, {"add", "-A"}} {
		if _, err := gitOutput(repo, args...); err != nil {
			return err
		}
	}
	cmd := osexec.Command("git", "-C", repo, "-c", "user.name=Threadmill benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "--quiet", "-m", "Frozen benchmark fixture")
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("commit fixture: %w: %s", err, out)
	}
	return nil
}
