package exec

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"golang.org/x/sys/unix"
)

var bwrapReadOnlyPaths = [...]string{
	"/usr", "/bin", "/lib", "/lib64",
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
	"/etc/ssl/certs", "/etc/pki",
}

func bwrapReadOnlyArgs() []string {
	args := make([]string, 0, 3*len(bwrapReadOnlyPaths))
	for _, path := range bwrapReadOnlyPaths {
		args = append(args, "--ro-bind-try", path, path)
	}
	return args
}

func (s *Scheduler) externalDependencyPath(path string) string {
	if s.sandbox != sandboxBwrap {
		return path
	}
	// --dev supplies the same fixed EOF device. Other virtual content remains
	// uncacheable, including random devices and process-dependent proc files.
	if path == "/dev/null" {
		var stat unix.Stat_t
		if err := unix.Stat(path, &stat); err == nil && stat.Mode&unix.S_IFMT == unix.S_IFCHR &&
			unix.Major(uint64(stat.Rdev)) == 1 && unix.Minor(uint64(stat.Rdev)) == 3 {
			return path
		}
		return ""
	}
	for _, root := range bwrapReadOnlyPaths {
		if path == root {
			return path
		}
		if !strings.HasPrefix(path, root+"/") {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return path
		}
		for _, visible := range bwrapReadOnlyPaths {
			target, err := filepath.EvalSymlinks(visible)
			if err == nil && (resolved == target || strings.HasPrefix(resolved, target+"/")) {
				return path
			}
		}
		return ""
	}
	return ""
}

func (s *Scheduler) externalDependencyType(path string) string {
	if s.sandbox != sandboxBwrap {
		return ""
	}
	// These types come from the sandbox's own tmpfs, --dev and --proc mounts.
	// Content reads still require a real backing path and cannot use this mapping.
	switch path {
	case "/", "/etc", "/home", "/dev", "/proc", "/proc/self", "/proc/thread-self":
		return "dir"
	case "/dev/tty", "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/ptmx":
		return "other"
	}
	if name, ok := strings.CutPrefix(path, "/proc/"); ok {
		if pid, err := strconv.Atoi(name); err == nil && pid > 0 && strconv.Itoa(pid) == name {
			return "dir"
		}
	}
	return ""
}

const (
	bwrapWorkspace = "/workspace"
	sandboxHome    = "/home/threadmill"
)

func probeBwrap() bool {
	if _, err := osexec.LookPath("bwrap"); err != nil {
		return false
	}
	dir, err := os.MkdirTemp("", "threadmill-bwrap-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o750); err != nil {
		return false
	}
	args := []string{
		"--unshare-user",
		"--unshare-pid",
		"--die-with-parent",
		"--tmpfs", "/",
		"--dir", bwrapWorkspace,
		"--bind", dir, bwrapWorkspace,
		"--bind", filepath.Join(dir, "tmp"), "/tmp",
	}
	args = append(args, bwrapReadOnlyArgs()...)
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", bwrapWorkspace,
		"--",
		"bash", "-c", "true",
	)
	cmd := osexec.Command("bwrap", args...)
	return cmd.Run() == nil
}

func runBwrap(
	ctx context.Context,
	live, tempDir, command string,
	capBytes int,
	track func(int),
	trace *traceRun,
) (env.ExecResult, error) {
	// 追踪器运行在同一个隔离 PID 命名空间内；-D 保持前台命令独立退出。
	args := trace.wrap(bashArgs(command))
	bwrapArgs := []string{
		"--unshare-user",
		"--unshare-pid",
		"--die-with-parent",
		"--tmpfs", "/",
		"--dir", bwrapWorkspace,
		"--bind", live, bwrapWorkspace,
		"--bind", filepath.Join(tempDir, "home"), sandboxHome,
		"--bind", filepath.Join(tempDir, "tmp"), "/tmp",
	}
	bwrapArgs = append(bwrapArgs, bwrapReadOnlyArgs()...)
	bwrapArgs = append(bwrapArgs,
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", bwrapWorkspace,
		"--",
	)
	bwrapArgs = append(bwrapArgs, args...)
	cmd := osexec.CommandContext(ctx, "bwrap", bwrapArgs...)
	cmd.Env = networkSandboxEnv(sandboxHome, "/tmp")
	return collect(ctx, cmd, capBytes, track)
}
