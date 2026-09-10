package vfs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	floorDirName  = ".floor"
	floorMetaName = ".floor.json"
	floorTempName = ".floor-staging"
)

// floorMeta records which display-surface state the floor was cloned from.
type floorMeta struct {
	DisplayDigest string `json:"display_digest"`
	Floor         string `json:"floor,omitempty"`
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
// An outside project edit creates another immutable floor. Existing floors and
// environments remain available because retained tasks may still reference them.
func prepareFloor(displayDir, liveRoot string) (string, error) {
	metaPath := filepath.Join(liveRoot, floorMetaName)
	digest, err := displayDigest(displayDir)
	if err != nil {
		return "", err
	}
	if floor, ok := matchingFloor(liveRoot, metaPath, digest); ok {
		return floor, nil
	}
	floor := filepath.Join(liveRoot, ".floors", digest)
	if info, err := os.Stat(floor); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("vfs: floor is not a directory: %s", floor)
		}
	} else if os.IsNotExist(err) {
		if err := cloneFloor(displayDir, floor, liveRoot); err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("vfs: inspect floor: %w", err)
	}
	if err := writeFloorMeta(metaPath, digest, floor); err != nil {
		return "", err
	}
	return floor, nil
}

func matchingFloor(liveRoot, metaPath, digest string) (string, bool) {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return "", false
	}
	var meta floorMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", false
	}
	if meta.DisplayDigest == "" || meta.DisplayDigest != digest {
		return "", false
	}
	floor := meta.Floor
	if floor == "" {
		floor = filepath.Join(liveRoot, floorDirName)
	}
	root, err := confinedRoot(floor)
	if err != nil || escapesRoot(liveRoot, root) {
		return "", false
	}
	info, err := os.Stat(root)
	return root, err == nil && info.IsDir()
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
	if err := os.MkdirAll(filepath.Dir(floor), 0o700); err != nil {
		return fmt.Errorf("vfs: create floor directory: %w", err)
	}
	if err := os.Rename(staging, floor); err != nil {
		return fmt.Errorf("vfs: install floor: %w", err)
	}
	return nil
}

func writeFloorMeta(metaPath, digest, floor string) error {
	payload, err := json.Marshal(floorMeta{DisplayDigest: digest, Floor: floor})
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
			"%s|%s|%d|%d|%o\n",
			slashed,
			info.Mode().Type().String(),
			info.Size(),
			info.ModTime().UnixNano(),
			info.Mode().Perm(),
		)
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("vfs: digest project: %w", walkErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
