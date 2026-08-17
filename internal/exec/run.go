package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"syscall"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func sandboxEnv(live string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + live,
		"LANG=C.UTF-8",
	}
}

func collect(ctx context.Context, cmd *osexec.Cmd, capBytes int) (env.ExecResult, error) {
	var buf capBuffer
	buf.cap = capBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return env.ExecResult{}, err
	}
	pgid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	var err error
	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err = <-waited
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	out := buf.String()
	if ctx.Err() != nil {
		return env.ExecResult{Output: out}, ctx.Err()
	}
	if err == nil {
		return env.ExecResult{Output: out}, nil
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		return env.ExecResult{ExitCode: ee.ExitCode(), Output: out}, nil
	}
	return env.ExecResult{Output: out}, err
}
