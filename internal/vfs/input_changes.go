package vfs

import (
	"fmt"
	"slices"
)

const (
	InputChangeAdded    = "added"
	InputChangeModified = "modified"
	InputChangeDeleted  = "deleted"
)

// InputChange describes one direct candidate change relative to its base snapshot.
// Reading changes never mutates another environment.
type InputChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// InputApplyResult reports the paths accepted or rejected by one atomic apply.
type InputApplyResult struct {
	Applied   []string `json:"applied"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// InputChanges returns the candidate's direct delta in stable path order.
func (s *Store) InputChanges(candidateID string) ([]InputChange, error) {
	if err := s.Absorb(candidateID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputChangesLocked(candidateID), nil
}

func (s *Store) inputChangesLocked(candidateID string) []InputChange {
	candidate := s.envs[candidateID]
	if candidate == nil {
		return []InputChange{}
	}
	paths := make([]string, 0, len(candidate.files))
	for path := range candidate.files {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	changes := make([]InputChange, 0, len(paths))
	for _, path := range paths {
		item := candidate.files[path]
		base := s.baseContent(candidate, candidate.baseID, path)
		kind := InputChangeModified
		switch {
		case item.tombstone:
			kind = InputChangeDeleted
		case !base.exists || base.tombstone:
			kind = InputChangeAdded
		}
		changes = append(changes, InputChange{Path: path, Kind: kind})
	}
	return changes
}

// ApplyInput explicitly adopts selected candidate paths into targetID. Safe mode
// rejects the whole request when any selected path conflicts; replace mode uses
// the candidate versions intentionally. An empty path list selects every change.
func (s *Store) ApplyInput(
	candidateID, targetID string,
	paths []string,
	replace bool,
) (InputApplyResult, error) {
	if candidateID == "" || targetID == "" || candidateID == targetID {
		return InputApplyResult{}, fmt.Errorf("vfs: invalid input environments")
	}
	if err := s.Absorb(candidateID); err != nil {
		return InputApplyResult{}, err
	}
	if err := s.Absorb(targetID); err != nil {
		return InputApplyResult{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := s.envs[candidateID]
	if candidate == nil {
		return InputApplyResult{}, fmt.Errorf("vfs: input candidate %q does not exist", candidateID)
	}
	changes := s.inputChangesLocked(candidateID)
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
				return InputApplyResult{}, err
			}
			if _, ok := available[clean]; !ok {
				return InputApplyResult{}, fmt.Errorf("vfs: path %q is not a candidate change", clean)
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
		return InputApplyResult{Applied: []string{}}, nil
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
		planned, conflicts := s.inputPlanLocked(candidateID, targetID)
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
			return InputApplyResult{Conflicts: selectedConflicts}, nil
		}
		for _, item := range planned {
			if _, ok := selectedSet[item.path]; ok {
				apply = append(apply, item)
			}
		}
	}

	if err := s.applyPendingLocked(targetID, apply); err != nil {
		return InputApplyResult{}, err
	}
	return InputApplyResult{Applied: selected}, nil
}
