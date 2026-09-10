package vfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Input holds the differing paths and read-only candidate environments for a
// prepared input. Candidates follow the source order; no differences means both
// slices are empty.
type Input struct {
	Candidates []string `json:"candidates"`
	Paths      []string `json:"paths"`
}

type inputRecord struct {
	Sources []string `json:"sources"`
	Input   Input    `json:"input"`
}

// PrepareInput initializes targetID with the complete state shared by all
// sources. A single source is inherited without producing any differences.
func (s *Store) PrepareInput(targetID string, sourceIDs []string) (Input, error) {
	if targetID == "" || len(sourceIDs) == 0 {
		return Input{}, fmt.Errorf("vfs: input requires a target and at least one source")
	}
	seen := make(map[string]struct{}, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if sourceID == "" || sourceID == targetID {
			return Input{}, fmt.Errorf("vfs: invalid input source %q", sourceID)
		}
		if _, duplicate := seen[sourceID]; duplicate {
			return Input{}, fmt.Errorf("vfs: duplicate input source %q", sourceID)
		}
		seen[sourceID] = struct{}{}
	}
	s.mu.Lock()
	if target := s.envs[targetID]; target != nil && target.input != nil {
		input, err := retryInput(target.input, sourceIDs)
		s.mu.Unlock()
		return input, err
	}
	s.mu.Unlock()
	record, err := s.readInput(targetID)
	if err != nil {
		return Input{}, err
	}
	if record != nil {
		if _, err := retryInput(record, sourceIDs); err != nil {
			return Input{}, err
		}
	}
	if err := s.Restore(targetID); err == nil {
		if record == nil {
			return Input{}, fmt.Errorf("vfs: input target %q already exists", targetID)
		}
	} else if !errors.Is(err, ErrUnknownEnvironment) {
		return Input{}, err
	}
	for _, sourceID := range sourceIDs {
		if err := s.Restore(sourceID); err != nil {
			return Input{}, err
		}
		if err := s.Absorb(sourceID); err != nil {
			return Input{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if target := s.envs[targetID]; target != nil && target.input != nil {
		return retryInput(target.input, sourceIDs)
	}
	if s.envs[targetID] != nil && record == nil {
		return Input{}, fmt.Errorf("vfs: input target %q already exists", targetID)
	}
	common := &layer{
		baseID:       sourceIDs[0],
		files:        make(map[string]blob),
		baseSnapshot: s.snapshotOverlays(sourceIDs[0]),
	}
	input := Input{Candidates: []string{}, Paths: []string{}}
	if len(sourceIDs) == 1 {
		return s.commitInput(targetID, sourceIDs, common, nil, input)
	}
	sources := make([]map[string]fileSnapshot, 0, len(sourceIDs))
	paths := make(map[string]struct{})
	for _, sourceID := range sourceIDs {
		files, err := s.visibleInputFiles(sourceID)
		if err != nil {
			return Input{}, err
		}
		sources = append(sources, files)
		for path := range files {
			paths[path] = struct{}{}
		}
	}
	for path := range paths {
		first, firstExists := sources[0][path]
		for _, source := range sources[1:] {
			other, otherExists := source[path]
			equal, err := inputFilesEqual(first, other)
			if err != nil {
				return Input{}, err
			}
			if firstExists != otherExists || !equal {
				input.Paths = append(input.Paths, path)
				break
			}
		}
	}
	slices.Sort(input.Paths)
	for _, path := range input.Paths {
		common.files[path] = blob{tombstone: true}
		allDirectories := true
		for _, source := range sources {
			if !source[path].mode.IsDir() {
				allDirectories = false
				break
			}
		}
		if allDirectories {
			// The directory is a shared container even when its permissions
			// differ. Keep its children readable until that metadata is chosen.
			common.files[path] = blob{mode: fs.ModeDir | 0o700}
		}
	}
	candidates := make([]*layer, 0, len(sourceIDs))
	if len(input.Paths) != 0 {
		for i, source := range sources {
			candidate := &layer{
				baseID:        targetID,
				completeInput: true,
				files:         make(map[string]blob, len(input.Paths)),
				baseSnapshot:  append([]map[string]blob{cloneFiles(common.files)}, common.baseSnapshot...),
			}
			for _, path := range input.Paths {
				file, ok := source[path]
				if !ok {
					candidate.files[path] = blob{tombstone: true}
					continue
				}
				data, err := snapshotContent(file)
				if err != nil {
					return Input{}, err
				}
				candidate.files[path] = blob{data: cloneBytes(data), mode: file.mode}
			}
			input.Candidates = append(input.Candidates, fmt.Sprintf("input:%s:%d", persistentID(targetID), i))
			candidates = append(candidates, candidate)
		}
	}
	return s.commitInput(targetID, sourceIDs, common, candidates, input)
}

// commitInput publishes all logical environments only after the reconstructible
// input manifest is durable. A resumed target keeps its existing file decisions.
func (s *Store) commitInput(
	targetID string,
	sourceIDs []string,
	common *layer,
	candidates []*layer,
	input Input,
) (Input, error) {
	for _, id := range input.Candidates {
		if _, exists := s.envs[id]; exists {
			return Input{}, fmt.Errorf("vfs: input candidate %q already exists", id)
		}
	}
	record := &inputRecord{Sources: slices.Clone(sourceIDs), Input: cloneInput(input)}
	if err := s.writeInput(targetID, record); err != nil {
		return Input{}, err
	}
	if target := s.envs[targetID]; target != nil {
		target.input = record
	} else {
		common.input = record
		s.envs[targetID] = common
	}
	for i, candidate := range candidates {
		s.envs[input.Candidates[i]] = candidate
	}
	return input, nil
}

func (s *Store) inputPath(targetID string) string {
	return filepath.Join(s.liveRoot, ".input-"+persistentID(targetID)+".json")
}

func (s *Store) readInput(targetID string) (*inputRecord, error) {
	if s.liveRoot == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.inputPath(targetID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vfs: read input: %w", err)
	}
	var record inputRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("vfs: decode input: %w", err)
	}
	return &record, nil
}

func (s *Store) writeInput(targetID string, record *inputRecord) error {
	if s.liveRoot == "" {
		return nil
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("vfs: encode input: %w", err)
	}
	path := s.inputPath(targetID)
	file, err := os.CreateTemp(s.liveRoot, ".input-tmp-")
	if err != nil {
		return fmt.Errorf("vfs: create input: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close(), os.Remove(file.Name()))
	}
	if err := file.Close(); err != nil {
		return errors.Join(err, os.Remove(file.Name()))
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.Join(err, os.Remove(file.Name()))
	}
	return nil
}

func (s *Store) visibleInputFiles(envID string) (map[string]fileSnapshot, error) {
	files, err := s.visibleRegularFiles(envID)
	if err != nil {
		return nil, err
	}
	directories, err := s.visibleDirectories(envID)
	if err != nil {
		return nil, err
	}
	for path := range directories {
		state := s.lookupContent(envID, path)
		mode := state.mode
		if !mode.IsDir() {
			mode = fs.ModeDir | 0o750
		}
		files[path] = fileSnapshot{mode: mode}
	}
	return files, nil
}

func retryInput(record *inputRecord, sources []string) (Input, error) {
	if record == nil || !slices.Equal(record.Sources, sources) {
		return Input{}, fmt.Errorf("vfs: target already exists with a different input")
	}
	return cloneInput(record.Input), nil
}

func cloneInput(input Input) Input {
	return Input{Candidates: slices.Clone(input.Candidates), Paths: slices.Clone(input.Paths)}
}

func inputFilesEqual(a, b fileSnapshot) (bool, error) {
	if a.mode != b.mode {
		return false, nil
	}
	if a.source != "" && a.source == b.source {
		return true, nil
	}
	left, err := snapshotContent(a)
	if err != nil {
		return false, err
	}
	right, err := snapshotContent(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}
