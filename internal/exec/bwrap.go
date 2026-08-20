package exec

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"

	"github.com/KDZZZZZZ/threadmill/internal/env"
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
	cmd := osexec.Command(
		"bwrap",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-net",
		"--die-with-parent",
		"--bind", dir, "/",
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", "/",
		"--",
		"bash", "-c", "true",
	)
	return cmd.Run() == nil
}

func runBwrap(ctx context.Context, live, command string, capBytes int, track func(int)) (env.ExecResult, error) {
	args := bashArgs(command)
	cmd := osexec.CommandContext(
		ctx,
		"bwrap",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-net",
		"--die-with-parent",
		"--bind", live, "/",
		"--ro-bind-try", "/usr", "/usr",
		"--ro-bind-try", "/bin", "/bin",
		"--ro-bind-try", "/lib", "/lib",
		"--ro-bind-try", "/lib64", "/lib64",
		"--dev", "/dev",
		"--proc", "/proc",
		"--chdir", "/",
		"--",
		args[0], args[1], args[2],
	)
	cmd.Env = sandboxEnv("/", "/tmp")
	return collect(ctx, cmd, capBytes, track)
}
