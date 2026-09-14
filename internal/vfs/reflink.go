package vfs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ReflinkSupported tests CoW cloning on root using nonempty files. Filesystem
// names alone cannot prove support (for example XFS may disable reflinks).
func ReflinkSupported(root string) bool {
	return requireReflink(root, root) == nil
}

// ReflinkCloneable tests the actual source/destination pair, including mount
// boundaries and the current user's permissions.
func ReflinkCloneable(floorDir, liveRoot string) bool {
	return requireReflink(floorDir, liveRoot) == nil
}

func requireReflink(sourceDir, targetDir string) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("vfs: reflink required from %q to %q; ordinary copying is disabled: %w", sourceDir, targetDir, retErr)
		}
	}()
	source, err := os.CreateTemp(sourceDir, ".threadmill-reflink-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, source.Close(), os.Remove(source.Name())) }()
	if _, err := source.WriteString("threadmill CoW probe\n"); err != nil {
		return err
	}
	target, err := os.CreateTemp(targetDir, ".threadmill-reflink-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, target.Close(), os.Remove(target.Name())) }()
	// FICLONE never falls back to copying. See Linux ioctl_ficlone(2).
	if err := unix.IoctlFileClone(int(target.Fd()), int(source.Fd())); err != nil {
		return err
	}
	if _, err := target.WriteAt([]byte("changed"), 0); err != nil {
		return err
	}
	original, err := os.ReadFile(source.Name())
	if err != nil {
		return err
	}
	if string(original) != "threadmill CoW probe\n" {
		return fmt.Errorf("clone changes modified the source")
	}
	return nil
}
