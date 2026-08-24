package vfs

import (
	"fmt"
	"slices"
)

const (
	JoinChangeAdded    = "added"
	JoinChangeModified = "modified"
	JoinChangeDeleted  = "deleted"
)

// JoinChange describes one direct candidate change relative to its fork point.
// Reading changes never mutates another environment.
type JoinChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// JoinApplyResult reports the paths accepted or rejected by one atomic apply.
type JoinApplyResult struct {
	Applied   []string `json:"applied"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// JoinChanges returns the candidate's direct delta in stable path order.
func (s *Store) JoinChanges(candidateID string) ([]JoinChange, error) {
	if err := s.Absorb(candidateID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.joinChangesLocked(candidateID), nil
}

func (s *Store) joinChangesLocked(candidateID string) []JoinChange {
	candidate := s.envs[candidateID]
	if candidate == nil {
		return []JoinChange{}
	}
	paths := make([]string, 0, len(candidate.files))
	for path := range candidate.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	changes := make([]JoinChange, 0, len(paths))
	for _, path := range paths {
		item := candidate.files[path]
		base := s.mergeBase(candidate, candidate.parentID, path)
		kind := JoinChangeModified
		switch {
		case item.tombstone:
			kind = JoinChangeDeleted
		case !base.exists || base.tombstone:
			kind = JoinChangeAdded
		}
		changes = append(changes, JoinChange{Path: path, Kind: kind})
	}
	return changes
}

// ApplyJoin explicitly adopts selected candidate paths into targetID. Safe mode
// rejects the whole request when any selected path conflicts; replace mode uses
// the candidate versions intentionally. An empty path list selects every change.
func (s *Store) ApplyJoin(
	candidateID, targetID string,
	paths []string,
	replace bool,
) (JoinApplyResult, error) {
	if candidateID == "" || targetID == "" || candidateID == targetID {
		return JoinApplyResult{}, fmt.Errorf("vfs: invalid join environments")
	}
	if err := s.Absorb(candidateID); err != nil {
		return JoinApplyResult{}, err
	}
	if err := s.Absorb(targetID); err != nil {
		return JoinApplyResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := s.envs[candidateID]
	if candidate == nil {
		return JoinApplyResult{}, fmt.Errorf("vfs: join candidate %q does not exist", candidateID)
	}
	changes := s.joinChangesLocked(candidateID)
	available := make(map[string]struct{}, len(changes))
	for _, change := range changes {
		available[change.Path] = struct{}{}
	}

	selected := make([]string, 0, len(paths))
	if len(paths) == 0 {
		for _, change := range changes {
			selected = append(selected, change.Path)
		}
	} else {
		seen := make(map[string]struct{}, len(paths))
		for _, path := range paths {
			clean, err := jail(path)
			if err != nil {
				return JoinApplyResult{}, err
			}
			if _, ok := available[clean]; !ok {
				return JoinApplyResult{}, fmt.Errorf("vfs: path %q is not a candidate change", clean)
			}
			if _, duplicate := seen[clean]; duplicate {
				continue
			}
			seen[clean] = struct{}{}
			selected = append(selected, clean)
		}
		slices.Sort(selected)
	}
	if len(selected) == 0 {
		return JoinApplyResult{Applied: []string{}}, nil
	}

	selectedSet := make(map[string]struct{}, len(selected))
	for _, path := range selected {
		selectedSet[path] = struct{}{}
	}

	var apply []pending
	if replace {
		apply = make([]pending, 0, len(selected))
		for _, path := range selected {
			apply = append(apply, pending{path: path, b: cloneBlob(candidate.files[path])})
		}
	} else {
		planned, conflicts := s.mergePlanLocked(candidateID, targetID)
		selectedConflicts := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			for path := range selectedSet {
				if pathsOverlap(path, conflict) {
					selectedConflicts = append(selectedConflicts, conflict)
					break
				}
			}
		}
		if len(selectedConflicts) > 0 {
			return JoinApplyResult{Conflicts: selectedConflicts}, nil
		}
		for _, item := range planned {
			if _, ok := selectedSet[item.path]; ok {
				apply = append(apply, item)
			}
		}
	}

	if err := s.applyPendingLocked(targetID, apply); err != nil {
		return JoinApplyResult{}, err
	}
	return JoinApplyResult{Applied: selected}, nil
}
