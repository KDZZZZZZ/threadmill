//go:build linux

package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const (
	mountNamespaceHelperArg  = "__threadmill_exec_mount_namespace"
	mountNamespaceHelperExit = 253
	mountNamespaceError      = "threadmill: workspace mount namespace: "
)

var (
	externalWorkspaceProbeOnce sync.Once
	externalWorkspaceProbeOK   bool
)

func init() {
	if len(os.Args) < 2 || os.Args[1] != mountNamespaceHelperArg {
		return
	}
	if err := mountNamespaceExec(os.Args[2:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, mountNamespaceError+err.Error())
		os.Exit(mountNamespaceHelperExit)
	}
}

func runExternalWorkspaceSandbox(
	ctx context.Context,
	live, workspace, tempDir, command string,
	capBytes int,
	track func(int),
	trace *traceRun,
) (env.ExecResult, error) {
	args := trace.wrap(bashArgs(command))
	program, err := osexec.LookPath(args[0])
	if err != nil {
		return env.ExecResult{}, fmt.Errorf("%w: locate command runner: %v", ErrWorkspaceIsolationUnavailable, err)
	}
	args[0], err = filepath.Abs(program)
	if err != nil {
		return env.ExecResult{}, fmt.Errorf("%w: resolve command runner: %v", ErrWorkspaceIsolationUnavailable, err)
	}
	executable, err := os.Executable()
	if err != nil {
		return env.ExecResult{}, fmt.Errorf("%w: locate helper: %v", ErrWorkspaceIsolationUnavailable, err)
	}
	helperArgs := make([]string, 0, len(args)+3)
	helperArgs = append(helperArgs, mountNamespaceHelperArg, live, workspace)
	helperArgs = append(helperArgs, args...)
	cmd := osexec.CommandContext(ctx, executable, helperArgs...)
	cmd.Dir = live
	cmd.Env = networkSandboxEnv(tempDir, tempDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWNS | unix.CLONE_NEWPID,
		Pdeathsig:  syscall.SIGKILL,
	}
	result, err := collect(ctx, cmd, capBytes, track)
	if err != nil {
		return result, err
	}
	if result.ExitCode == mountNamespaceHelperExit && strings.HasPrefix(result.Output, mountNamespaceError) {
		return result, fmt.Errorf("%w: %s", ErrWorkspaceIsolationUnavailable, strings.TrimSpace(result.Output))
	}
	return result, nil
}

func mountNamespaceExec(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("invalid helper arguments")
	}
	live, workspace, program := args[0], args[1], args[2]
	if !filepath.IsAbs(live) || !filepath.IsAbs(workspace) || !filepath.IsAbs(program) {
		return fmt.Errorf("live, workspace, and program must be absolute paths")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	if err := unix.Mount(live, workspace, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind live workspace: %w", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount private proc: %w", err)
	}
	if err := os.Chdir(workspace); err != nil {
		return fmt.Errorf("enter workspace: %w", err)
	}
	if err := dropMountCapability(); err != nil {
		return err
	}
	return syscall.Exec(program, args[2:], os.Environ())
}

func dropMountCapability() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}
	if err := unix.Prctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_CLEAR_ALL,
		0,
		0,
		0,
	); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	if err := unix.Prctl(unix.PR_CAPBSET_DROP, unix.CAP_SYS_ADMIN, 0, 0, 0); err != nil {
		return fmt.Errorf("drop mount capability from bounding set: %w", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read capabilities: %w", err)
	}
	word := unix.CAP_SYS_ADMIN / 32
	bit := uint32(1) << (unix.CAP_SYS_ADMIN % 32)
	data[word].Effective &^= bit
	data[word].Permitted &^= bit
	data[word].Inheritable &^= bit
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("drop mount capability: %w", err)
	}
	return nil
}

func probeExternalWorkspaceIsolation() bool {
	externalWorkspaceProbeOnce.Do(func() {
		root, err := os.MkdirTemp("", "threadmill-mountns-probe-")
		if err != nil {
			return
		}
		defer os.RemoveAll(root)
		live := filepath.Join(root, "live")
		workspace := filepath.Join(root, "workspace")
		tempDir := filepath.Join(root, "tmp")
		for _, dir := range []string{live, workspace, tempDir} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return
			}
		}
		if err := os.WriteFile(filepath.Join(live, "marker"), []byte("live"), 0o600); err != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(workspace, "marker"), []byte("base"), 0o600); err != nil {
			return
		}
		result, err := runExternalWorkspaceSandbox(
			context.Background(), live, workspace, tempDir,
			`test "$PWD" = `+shellSingleQuote(workspace)+` && test "$(cat marker)" = live`,
			4096, nil, nil,
		)
		externalWorkspaceProbeOK = err == nil && result.ExitCode == 0
	})
	return externalWorkspaceProbeOK
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
