package vfs

import (
	"errors"
	"fmt"
)

// BindProject makes the project directory this environment's live workspace.
// The caller prepares its input first and stops its commands before unbinding.
func (s *Store) BindProject(envID string) error {
	if s.liveRoot == "" {
		return fmt.Errorf("vfs: real directory requires a persistent store with an independent floor")
	}
	root, err := s.WorkspaceRoot()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if envID == "" {
		return ErrUnknownEnvironment
	}
	if s.projectEnv == envID {
		return nil
	}
	if s.projectEnv != "" {
		return fmt.Errorf("vfs: project is in use by %q", s.projectEnv)
	}
	if s.lives[envID] != "" || s.materializing[envID] != nil {
		return fmt.Errorf("vfs: environment %q already has a live workspace", envID)
	}
	s.ensure(envID)
	s.projectEnv = envID
	s.lives[envID] = root
	return nil
}

// ArchiveProject captures existing project files as an ordinary immutable input.
func (s *Store) ArchiveProject(ref string) (err error) {
	capture := ref + ":capture"
	if err := s.BindProject(capture); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Discard(capture)) }()
	return s.Archive(capture, ref)
}
