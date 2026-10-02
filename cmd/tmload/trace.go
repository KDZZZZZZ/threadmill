package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"path"
	"regexp"
	"strings"
)

// Trace v1 is shared by tmload and the pinned Pi native-tool harness.
// Fork, collect and release are compulsory for each agent, outside Operations.
type loadTrace struct {
	Version int          `json:"version"`
	Seed    int64        `json:"seed"`
	Fixture traceFixture `json:"fixture"`
	Serial  bool         `json:"serial,omitempty"`
	Agents  []traceAgent `json:"agents"`
}

type traceFixture struct {
	Kind      string `json:"kind"`
	Files     int    `json:"files"`
	FileBytes int    `json:"file_bytes"`
	Commit    string `json:"commit,omitempty"`
}

type traceAgent struct {
	ID         string    `json:"id"`
	Operations []traceOp `json:"operations"`
}

type traceOp struct {
	Op            string `json:"op"`
	DurationNS    int64  `json:"duration_ns,omitempty"`
	Path          string `json:"path,omitempty"`
	Content       string `json:"content,omitempty"`
	Command       string `json:"command,omitempty"`
	ExpectedExit  int    `json:"expected_exit,omitempty"`
	ExpectedCache *bool  `json:"expected_cache,omitempty"`
}

type traceGeneration struct {
	Agents, Turns, Files, FileBytes int
	Seed                            int64
	Think, Scale                    float64
	Paths                           []string
	Commit                          string
}

func generateTrace(cfg traceGeneration) (loadTrace, error) {
	if cfg.Agents < 1 || cfg.Turns < 1 || cfg.Files < 1 ||
		cfg.Think < 0 || cfg.Scale <= 0 || math.IsNaN(cfg.Think) || math.IsNaN(cfg.Scale) ||
		math.IsInf(cfg.Think, 0) || math.IsInf(cfg.Scale, 0) {
		return loadTrace{}, fmt.Errorf("invalid trace generation parameters")
	}
	trace := loadTrace{Version: 1, Seed: cfg.Seed,
		Fixture: traceFixture{Kind: "synthetic", Files: cfg.Files, FileBytes: cfg.FileBytes, Commit: cfg.Commit}}
	if len(cfg.Paths) > 0 {
		trace.Fixture.Kind = "repository"
		trace.Fixture.Files = len(cfg.Paths)
	}
	for id := range cfg.Agents {
		rng := rand.New(rand.NewSource(cfg.Seed + int64(id) + 1))
		agent := traceAgent{ID: fmt.Sprintf("agent-%d", id)}
		nextPath := func() string {
			if len(cfg.Paths) > 0 {
				return cfg.Paths[rng.Intn(len(cfg.Paths))]
			}
			return fixturePath(rng, cfg.Files)
		}
		for range cfg.Turns {
			agent.Operations = append(agent.Operations, traceOp{
				Op: "think", DurationNS: int64(sampleThink(rng) * 1e9 * cfg.Think),
			})
			for range rng.Intn(3) {
				agent.Operations = append(agent.Operations, traceOp{Op: "read", Path: nextPath()})
			}
			if rng.Float64() < 0.25 {
				agent.Operations = append(agent.Operations, traceOp{Op: "list", Path: "."})
			}
			if rng.Float64() < 0.25 {
				agent.Operations = append(agent.Operations, traceOp{Op: "write", Path: nextPath(), Content: string(payload(rng))})
			}
			if rng.Float64() < 0.6 {
				agent.Operations = append(agent.Operations, traceOp{Op: "bash", Command: sampleCommand(rng, cfg.Scale)})
			}
		}
		trace.Agents = append(trace.Agents, agent)
	}
	return trace, validateTrace(trace)
}

func decodeTrace(r io.Reader) (loadTrace, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	var trace loadTrace
	if err := decoder.Decode(&trace); err != nil {
		return trace, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return trace, fmt.Errorf("trace must contain exactly one JSON object")
	}
	return trace, validateTrace(trace)
}

var traceID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var commitID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateTrace(trace loadTrace) error {
	if trace.Version != 1 || len(trace.Agents) == 0 {
		return fmt.Errorf("trace requires version 1 and at least one agent")
	}
	if trace.Fixture.Kind != "synthetic" && trace.Fixture.Kind != "repository" {
		return fmt.Errorf("unknown fixture kind %q", trace.Fixture.Kind)
	}
	if trace.Fixture.Commit != "" && !commitID.MatchString(trace.Fixture.Commit) {
		return fmt.Errorf("fixture commit must be a full SHA-1")
	}
	seen := map[string]bool{}
	for _, agent := range trace.Agents {
		if !traceID.MatchString(agent.ID) || seen[agent.ID] {
			return fmt.Errorf("invalid or duplicate agent ID %q", agent.ID)
		}
		seen[agent.ID] = true
		if agent.Operations == nil {
			return fmt.Errorf("%s requires an operations array", agent.ID)
		}
		for n, op := range agent.Operations {
			switch op.Op {
			case "think":
				if op.DurationNS < 0 {
					return fmt.Errorf("%s operation %d: negative think duration", agent.ID, n)
				}
			case "read", "write", "list":
				clean := path.Clean(op.Path)
				traversal := false
				for _, part := range strings.Split(op.Path, "/") {
					traversal = traversal || part == ".."
				}
				if op.Path == "" || path.IsAbs(op.Path) || clean == ".." ||
					traversal || strings.HasPrefix(clean, "../") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
					return fmt.Errorf("%s operation %d: unsafe workspace path %q", agent.ID, n, op.Path)
				}
			case "bash":
				if op.Command == "" || op.ExpectedExit < 0 || op.ExpectedExit > 255 {
					return fmt.Errorf("%s operation %d: invalid bash operation", agent.ID, n)
				}
			default:
				return fmt.Errorf("%s operation %d: unknown operation %q", agent.ID, n, op.Op)
			}
		}
	}
	return nil
}
