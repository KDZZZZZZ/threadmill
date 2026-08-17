package exec

import (
	"context"
	"os"
	osexec "os/exec"

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
	lower := dir + "/lower"
	upper := dir + "/upper"
	work := dir + "/work"
	for _, p := range []string{lower, upper, work} {
		if err := os.Mkdir(p, 0o750); err != nil {
			return false
		}
	}
	cmd := osexec.Command(
		"bwrap",
		"--unshare-user",
		"--unshare-net",
		"--overlay-src", lower,
		"--overlay", upper, work, "/dest",
		"true",
	)
	return cmd.Run() == nil
}

func runBwrap(ctx context.Context, live, command string, capBytes int) (env.ExecResult, error) {
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
		"bash", "-c", command,
	)
	cmd.Env = sandboxEnv(live)
	return collect(ctx, cmd, capBytes)
}
