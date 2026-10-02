package cmdcache

import (
	"strings"
	"time"
)

// RoleStats contains fixed-role counters without task IDs or command contents.
type RoleStats struct {
	Lookups          uint64        `json:"lookups"`
	Hits             uint64        `json:"hits"`
	Stores           uint64        `json:"stores"`
	Replays          uint64        `json:"replays"`
	Executions       uint64        `json:"executions"`
	MatchedDuration  time.Duration `json:"matched_duration"`
	SavedDuration    time.Duration `json:"saved_duration"`
	ExecutedDuration time.Duration `json:"executed_duration"`
}

func boundedRole(role string) string {
	switch role {
	case "manager", "planner", "executor", "verifier", "input", "benchmark":
		return role
	default:
		return "unknown"
	}
}

func readKindName(kind ReadKind) string {
	switch kind {
	case ReadAbsent:
		return "absent"
	case ReadDir:
		return "dir"
	case ReadStat:
		return "stat"
	default:
		return "file"
	}
}

func dependencyKind(rel, state string) string {
	prefix := "workspace_"
	if strings.HasPrefix(rel, "~/") {
		prefix = "home_"
	}
	switch {
	case state == stateAbsent:
		return prefix + "absent"
	case strings.HasPrefix(state, "t:"):
		return prefix + "stat"
	case strings.HasPrefix(state, "d:"):
		return prefix + "dir"
	default:
		return prefix + "file"
	}
}

func (o Observation) rejectionReason() string {
	switch {
	case o.Incomplete:
		return "trace_incomplete"
	case o.Network:
		return "network"
	case len(o.Escaped) > 0:
		for _, name := range o.Escaped {
			if strings.HasPrefix(name, "~/") {
				return "home_write"
			}
		}
		return "external_write"
	case o.rewritesOwnInput():
		return "input_rewrite"
	default:
		return ""
	}
}

// Reject attributes a refusal to store. Unknown labels collapse to one bucket.
func (c *Cache) Reject(reason string) {
	if c == nil {
		return
	}
	switch reason {
	case "trace_incomplete", "trace_unavailable", "run_error", "network", "home_write", "external_write", "input_rewrite", "read_set_limit", "failure_disabled", "unreadable_dependency", "unreadable_external", "artifact_capture", "index_write":
	default:
		reason = "other"
	}
	c.mu.Lock()
	c.stats.Rejected++
	c.stats.RejectedReasons[reason]++
	c.mu.Unlock()
}

// RecordExecution measures real execution, separating audited hits from misses.
func (c *Cache) RecordExecution(role string, duration time.Duration, verification ...bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.ExecutedDuration += duration
	if len(verification) > 0 && verification[0] {
		c.stats.VerificationDuration += duration
	}
	name := boundedRole(role)
	stats := c.stats.Roles[name]
	stats.Executions++
	stats.ExecutedDuration += duration
	c.stats.Roles[name] = stats
}

func (c *Cache) recordTiming(lookup bool, duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lookup {
		c.stats.LookupDuration += duration
		c.stats.LookupMaxDuration = max(c.stats.LookupMaxDuration, duration)
	} else {
		c.stats.StoreDuration += duration
	}
}

func (c *Cache) recordMiss(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case "no_key":
		c.stats.MissNoKey++
	case "read_set":
		c.stats.MissReadSet++
	case "artifacts":
		c.stats.MissArtifacts++
	case "corrupt":
		c.stats.MissCorrupt++
	}
}
