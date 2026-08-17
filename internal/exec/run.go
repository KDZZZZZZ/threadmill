package exec

import (
	"context"
	"errors"
	osexec "os/exec"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func collect(ctx context.Context, cmd *osexec.Cmd, capBytes int) (env.ExecResult, error) {
	var buf capBuffer
	buf.cap = capBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
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
