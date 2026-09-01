package vfs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	fuseSuperMagic     = 0x65735546
	nativeOverlayMagic = 0x794c7630
)

// fuse-overlayfs invocation and layer roles follow the upstream manual:
// https://github.com/containers/fuse-overlayfs/blob/main/fuse-overlayfs.1.md
// Native lower/upper/work semantics follow the kernel documentation:
// https://www.kernel.org/doc/html/latest/filesystems/overlayfs.html

type overlayDriver struct {
	kind    string
	program string
	unmount string
}

type overlayMount struct {
	cmd         *exec.Cmd
	done        chan error
	driver      *overlayDriver
	mountpoint  string
	upperdir    string
	cleanupRoot string
	release     func()
}

type overlaySeedFile struct {
	Path       string `json:"path"`
	Data       []byte `json:"data,omitempty"`
	Tombstone  bool   `json:"tombstone,omitempty"`
	Executable bool   `json:"executable,omitempty"`
}

func detectOverlayDriver() *overlayDriver {
	if os.Geteuid() == 0 {
		return &overlayDriver{kind: "native-overlayfs"}
	}
	program, err := exec.LookPath("fuse-overlayfs")
	if err != nil {
		return nil
	}
	unmount, err := exec.LookPath("fusermount3")
	if err != nil {
		return nil
	}
	return &overlayDriver{kind: "fuse-overlayfs", program: program, unmount: unmount}
}

func (d *overlayDriver) mount(lower, upper, work, mountpoint string) (*overlayMount, error) {
	for _, path := range []string{lower, upper, work, mountpoint} {
		if strings.ContainsAny(path, ",:\\") {
			return nil, fmt.Errorf("vfs: overlay path contains an unsupported option separator: %q", path)
		}
	}
	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if d.kind == "native-overlayfs" {
		options += ",redirect_dir=off,metacopy=off,index=off"
		if err := syscall.Mount(
			"overlay",
			mountpoint,
			"overlay",
			syscall.MS_NODEV|syscall.MS_NOSUID,
			options,
		); err != nil {
			return nil, fmt.Errorf("vfs: mount native overlayfs: %w", err)
		}
		return &overlayMount{driver: d, mountpoint: mountpoint, upperdir: upper}, nil
	}
	var output bytes.Buffer
	cmd := exec.Command(d.program, "-f", "-o", options, mountpoint)
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vfs: start fuse-overlayfs: %w", err)
	}
	mount := &overlayMount{
		cmd:        cmd,
		done:       make(chan error, 1),
		driver:     d,
		mountpoint: mountpoint,
		upperdir:   upper,
	}
	go func() { mount.done <- cmd.Wait() }()

	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		if overlayMounted(mountpoint) {
			return mount, nil
		}
		select {
		case err := <-mount.done:
			return nil, fmt.Errorf("vfs: mount fuse-overlayfs: %w: %s", err, strings.TrimSpace(output.String()))
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-mount.done
			return nil, fmt.Errorf("vfs: mount fuse-overlayfs: timed out: %s", strings.TrimSpace(output.String()))
		case <-ticker.C:
		}
	}
}

func (m *overlayMount) close() error {
	if m == nil {
		return nil
	}
	if overlayMounted(m.mountpoint) {
		if m.driver.kind == "native-overlayfs" {
			if err := syscall.Unmount(m.mountpoint, 0); err != nil {
				return fmt.Errorf("vfs: unmount native overlayfs: %w", err)
			}
		} else {
			output, err := exec.Command(m.driver.unmount, "-u", m.mountpoint).CombinedOutput()
			if err != nil && overlayMounted(m.mountpoint) {
				return fmt.Errorf("vfs: unmount overlay: %w: %s", err, strings.TrimSpace(string(output)))
			}
		}
	}
	if m.cmd != nil {
		select {
		case <-m.done:
		case <-time.After(2 * time.Second):
			if err := m.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return fmt.Errorf("vfs: stop fuse-overlayfs: %w", err)
			}
			<-m.done
		}
	}
	if m.release != nil {
		m.release()
		m.release = nil
	}
	return nil
}

func overlayMounted(path string) bool {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return false
	}
	magic := uint64(stat.Type)
	return magic == fuseSuperMagic || magic == nativeOverlayMagic
}

func (s *Store) overlayStatePath(envID string) string {
	return filepath.Join(s.liveRoot, ".overlay-"+persistentID(envID))
}

func (s *Store) overlayStateExists(envID string) bool {
	if s.liveRoot == "" {
		return false
	}
	info, err := os.Stat(s.overlayStatePath(envID))
	return err == nil && info.IsDir()
}

func (s *Store) registerOverlay(envID string, mount *overlayMount) {
	s.mountMu.Lock()
	s.mounts[envID] = mount
	s.mountMu.Unlock()
}

func (s *Store) closeOverlay(envID string) error {
	s.mountMu.Lock()
	mount := s.mounts[envID]
	if mount != nil {
		delete(s.mounts, envID)
	}
	s.mountMu.Unlock()
	if mount == nil {
		return nil
	}
	if err := mount.close(); err != nil {
		s.mountMu.Lock()
		if s.mounts[envID] == nil {
			s.mounts[envID] = mount
		}
		s.mountMu.Unlock()
		return err
	}
	if mount.cleanupRoot != "" {
		if err := os.RemoveAll(mount.cleanupRoot); err != nil {
			return fmt.Errorf("vfs: remove overlay state: %w", err)
		}
	}
	return nil
}

func (s *Store) overlayActive() int {
	s.mountMu.Lock()
	defer s.mountMu.Unlock()
	return len(s.mounts)
}

func (s *Store) restoreOverlay(envID string) (string, bool, error) {
	if !s.overlayStateExists(envID) {
		return "", false, nil
	}
	if s.overlay == nil {
		return "", false, fmt.Errorf("vfs: persistent overlay for %q requires fuse-overlayfs and fusermount3", envID)
	}
	if !s.acquireOverlay() {
		return "", false, errOverlayCapacity
	}
	live := s.persistentLivePath(envID)
	if err := os.MkdirAll(live, 0o700); err != nil {
		s.releaseOverlay()
		return "", false, fmt.Errorf("vfs: restore overlay mountpoint: %w", err)
	}
	state := s.overlayStatePath(envID)
	if overlayMounted(live) {
		mount := &overlayMount{
			driver:     s.overlay,
			mountpoint: live,
			upperdir:   filepath.Join(state, "upper"),
			release:    s.releaseOverlay,
		}
		if err := completeOverlaySeed(live, state); err != nil {
			return "", false, errors.Join(err, mount.close())
		}
		s.registerOverlay(envID, mount)
		return live, true, nil
	}
	mount, err := s.overlay.mount(
		s.floorDir,
		filepath.Join(state, "upper"),
		filepath.Join(state, "work"),
		live,
	)
	if err != nil {
		s.releaseOverlay()
		return "", false, err
	}
	mount.release = s.releaseOverlay
	if err := completeOverlaySeed(live, state); err != nil {
		return "", false, errors.Join(err, mount.close())
	}
	s.registerOverlay(envID, mount)
	return live, true, nil
}

func (s *Store) createOverlay(
	envID, base string,
	blobs []overlayFile,
) (string, bool, error) {
	if s.overlay == nil {
		return "", false, nil
	}
	if !s.acquireOverlay() {
		return "", false, errOverlayCapacity
	}
	held := true
	defer func() {
		if held {
			s.releaseOverlay()
		}
	}()
	if s.liveRoot == "" {
		state, err := os.MkdirTemp("", "threadmill-overlay-")
		if err != nil {
			return "", false, err
		}
		live, mount, err := s.mountOverlayState(base, state, blobs)
		if err != nil {
			_ = os.RemoveAll(state)
			return "", false, err
		}
		mount.cleanupRoot = state
		mount.release = s.releaseOverlay
		held = false
		s.registerOverlay(envID, mount)
		return live, true, nil
	}

	state := s.overlayStatePath(envID)
	if _, err := os.Stat(state); err == nil {
		s.releaseOverlay()
		held = false
		return s.restoreOverlay(envID)
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	temporary, err := os.MkdirTemp(s.liveRoot, ".overlay-tmp-")
	if err != nil {
		return "", false, err
	}
	for _, dir := range []string{"upper", "work"} {
		if err := os.Mkdir(filepath.Join(temporary, dir), 0o700); err != nil {
			_ = os.RemoveAll(temporary)
			return "", false, err
		}
	}
	if err := writeOverlaySeed(temporary, blobs); err != nil {
		_ = os.RemoveAll(temporary)
		return "", false, err
	}
	if err := os.Rename(temporary, state); err != nil {
		_ = os.RemoveAll(temporary)
		if _, statErr := os.Stat(state); statErr != nil {
			return "", false, err
		}
		s.releaseOverlay()
		held = false
		return s.restoreOverlay(envID)
	}
	live := s.persistentLivePath(envID)
	if err := os.MkdirAll(live, 0o700); err != nil {
		return "", false, err
	}
	mount, err := s.overlay.mount(
		base,
		filepath.Join(state, "upper"),
		filepath.Join(state, "work"),
		live,
	)
	if err != nil {
		if cleanupErr := os.RemoveAll(live); cleanupErr != nil {
			return "", false, errors.Join(
				err,
				errOverlayCleanup,
				fmt.Errorf("vfs: remove failed overlay mountpoint: %w", cleanupErr),
			)
		}
		if cleanupErr := os.RemoveAll(state); cleanupErr != nil {
			return "", false, errors.Join(
				err,
				errOverlayCleanup,
				fmt.Errorf("vfs: remove failed overlay state: %w", cleanupErr),
			)
		}
		return "", false, err
	}
	mount.release = s.releaseOverlay
	held = false
	if err := completeOverlaySeed(live, state); err != nil {
		return "", false, errors.Join(err, mount.close())
	}
	s.registerOverlay(envID, mount)
	return live, true, nil
}

var (
	errOverlayCapacity = errors.New("vfs: overlay capacity reached")
	errOverlayCleanup  = errors.New("vfs: clean failed overlay state")
)

func (s *Store) acquireOverlay() bool {
	if s.overlaySlots == nil {
		return false
	}
	select {
	case s.overlaySlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Store) releaseOverlay() {
	<-s.overlaySlots
}

func (s *Store) mountOverlayState(
	base, state string,
	blobs []overlayFile,
) (string, *overlayMount, error) {
	upper := filepath.Join(state, "upper")
	work := filepath.Join(state, "work")
	live := filepath.Join(state, "merged")
	for _, dir := range []string{upper, work, live} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", nil, err
		}
	}
	mount, err := s.overlay.mount(base, upper, work, live)
	if err != nil {
		return "", nil, err
	}
	for _, item := range blobs {
		if err := applyLive(live, item.path, item.b); err != nil {
			return "", nil, errors.Join(err, mount.close())
		}
	}
	return live, mount, nil
}

func writeOverlaySeed(state string, blobs []overlayFile) error {
	seed := make([]overlaySeedFile, 0, len(blobs))
	for _, item := range blobs {
		seed = append(seed, overlaySeedFile{
			Path:       item.path,
			Data:       item.b.data,
			Tombstone:  item.b.tombstone,
			Executable: item.b.executable,
		})
	}
	data, err := json.Marshal(seed)
	if err != nil {
		return fmt.Errorf("vfs: encode overlay seed: %w", err)
	}
	if err := os.WriteFile(filepath.Join(state, "seed.json"), data, 0o600); err != nil {
		return fmt.Errorf("vfs: write overlay seed: %w", err)
	}
	return nil
}

func completeOverlaySeed(live, state string) error {
	path := filepath.Join(state, "seed.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vfs: read overlay seed: %w", err)
	}
	var seed []overlaySeedFile
	if err := json.Unmarshal(data, &seed); err != nil {
		return fmt.Errorf("vfs: decode overlay seed: %w", err)
	}
	for _, item := range seed {
		if err := applyLive(live, item.Path, blob{
			data:       item.Data,
			tombstone:  item.Tombstone,
			executable: item.Executable,
		}); err != nil {
			return fmt.Errorf("vfs: apply overlay seed: %w", err)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vfs: finish overlay seed: %w", err)
	}
	return nil
}

func persistentID(envID string) string {
	digest := sha256.Sum256([]byte(envID))
	return fmt.Sprintf("%x", digest)
}
