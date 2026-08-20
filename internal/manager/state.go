package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type statePaths struct {
	ProjectRoot     string
	GraphFile       string
	MemoryFile      string
	ManagerReactDir string
	ReactDir        string
	ProgressDir     string
	VFSDir          string
	LogFile         string
}

func openStatePaths(projectRoot string) (statePaths, error) {
	canonical, err := filepath.Abs(projectRoot)
	if err != nil {
		return statePaths{}, fmt.Errorf("resolve project root: %w", err)
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return statePaths{}, fmt.Errorf("resolve project root symlinks: %w", err)
	}
	canonical = filepath.Clean(canonical)

	home, err := os.UserHomeDir()
	if err != nil {
		return statePaths{}, fmt.Errorf("resolve threadmill home: %w", err)
	}
	digest := sha256.Sum256([]byte(filepath.ToSlash(canonical)))
	projectDir := filepath.Join(home, ".threadmill", "projects", hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return statePaths{}, fmt.Errorf("create project state directory %q: %w", projectDir, err)
	}
	return statePaths{
		ProjectRoot:     canonical,
		GraphFile:       filepath.Join(projectDir, "graphs", "coordination.json"),
		MemoryFile:      filepath.Join(projectDir, "graphs", "memory.json"),
		ManagerReactDir: filepath.Join(projectDir, "checkpoints", "manager"),
		ReactDir:        filepath.Join(projectDir, "checkpoints", "tasks"),
		ProgressDir:     filepath.Join(projectDir, "progress"),
		VFSDir:          filepath.Join(projectDir, "vfs"),
		LogFile:         filepath.Join(projectDir, "threadmill.log"),
	}, nil
}
