package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	floorDirName  = ".floor"
	floorMetaName = ".floor.json"
	floorTempName = ".floor-staging"
)

// floorMeta records which display-surface state the floor was cloned from.
type floorMeta struct {
	DisplayDigest string `json:"display_digest"`
}

// prepareFloor returns the immutable read floor for a persistent store.
//
// The project directory is a display surface: Publish renders checkpoints into
// it and the user may edit it between sessions. Environments must never read
// through to it, because an overlay lower directory may not change while an
// environment is mounted on it — that constraint, not publication itself, is
// what used to force publication to wait for a quiescent graph. The floor is
// therefore a private clone of the project, taken when a session adopts it.
//
// The clone is reused across restarts so a recovered session keeps reading the
// floor its environments were forked from. It is retaken only when the project
// no longer matches what the floor was cloned from — a publication some earlier
// session rendered, or an edit made outside Threadmill — because a new session
// has to build on what the user can actually see. Retaking it invalidates every
// persisted environment, so those are discarded with it.
func prepareFloor(displayDir, liveRoot string) (string, error) {
	floor := filepath.Join(liveRoot, floorDirName)
	metaPath := filepath.Join(liveRoot, floorMetaName)
	digest, err := displayDigest(displayDir)
	if err != nil {
		return "", err
	}
	if floorMatches(floor, metaPath, digest) {
		return floor, nil
	}
	if err := cloneFloor(displayDir, floor, liveRoot); err != nil {
		return "", err
	}
	// Environments forked from the previous floor read blobs that are absolute
	// content, not deltas; reviving one over a different floor would reinstate
	// pre-existing files. Discard before recording the new floor so a crash in
	// between simply retakes it.
	if err := discardStaleEnvironments(liveRoot, floor); err != nil {
		return "", err
	}
	if err := writeFloorMeta(metaPath, digest); err != nil {
		return "", err
	}
	return floor, nil
}

func floorMatches(floor, metaPath, digest string) bool {
	info, err := os.Stat(floor)
	if err != nil || !info.IsDir() {
		return false
	}
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var meta floorMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	return meta.DisplayDigest != "" && meta.DisplayDigest == digest
}

func cloneFloor(displayDir, floor, liveRoot string) error {
	source, err := confinedRoot(displayDir)
	if err != nil {
		return fmt.Errorf("vfs: open project for floor: %w", err)
	}
	staging := filepath.Join(liveRoot, floorTempName)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("vfs: reset floor staging: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("vfs: create floor staging: %w", err)
	}
	if _, err := copyTree(source, staging); err != nil {
		return fmt.Errorf("vfs: clone floor: %w", err)
	}
	if err := os.RemoveAll(floor); err != nil {
		return fmt.Errorf("vfs: replace floor: %w", err)
	}
	if err := os.Rename(staging, floor); err != nil {
		return fmt.Errorf("vfs: install floor: %w", err)
	}
	return nil
}

func writeFloorMeta(metaPath, digest string) error {
	payload, err := json.Marshal(floorMeta{DisplayDigest: digest})
	if err != nil {
		return fmt.Errorf("vfs: encode floor metadata: %w", err)
	}
	temp := metaPath + ".tmp"
	if err := os.WriteFile(temp, payload, 0o600); err != nil {
		return fmt.Errorf("vfs: write floor metadata: %w", err)
	}
	if err := os.Rename(temp, metaPath); err != nil {
		return fmt.Errorf("vfs: install floor metadata: %w", err)
	}
	return nil
}

// discardStaleEnvironments removes persisted environment workspaces and overlay
// state under liveRoot. Environment live directories are named by a hex digest
// of the environment ID and overlay state by an ".overlay-" prefix; nothing else
// under liveRoot belongs to an environment.
func discardStaleEnvironments(liveRoot, floor string) error {
	entries, err := os.ReadDir(liveRoot)
	if err != nil {
		return fmt.Errorf("vfs: scan persistent live root: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(liveRoot, name)
		if path == floor || !staleEnvironmentEntry(name) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("vfs: discard stale environment %q: %w", name, err)
		}
	}
	return nil
}

func staleEnvironmentEntry(name string) bool {
	if strings.HasPrefix(name, ".overlay-") {
		return true
	}
	if len(name) != 2*sha256.Size {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// displayDigest summarises the display surface by path, size and mtime. It skips
// .git because publication never renders into it, so ordinary git activity must
// not look like a project the session has to re-adopt.
func displayDigest(displayDir string) (string, error) {
	root, err := confinedRoot(displayDir)
	if err != nil {
		return "", fmt.Errorf("vfs: open project for floor digest: %w", err)
	}
	hasher := sha256.New()
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		if slashed == ".git" {
			return filepath.SkipDir
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		fmt.Fprintf(
			hasher,
			"%s|%s|%d|%d\n",
			slashed,
			info.Mode().Type().String(),
			info.Size(),
			info.ModTime().UnixNano(),
		)
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("vfs: digest project: %w", walkErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
