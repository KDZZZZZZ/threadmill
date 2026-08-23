package exec

import (
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"syscall"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func sandboxEnv(home, tmpdir string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + home,
		"TMPDIR=" + tmpdir,
		"LANG=C.UTF-8",
	}
}

var networkEnvironment = [...]string{
	"all_proxy",
	"http_proxy",
	"https_proxy",
	"no_proxy",
	"ALL_PROXY",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"NO_PROXY",
	"CURL_CA_BUNDLE",
	"GIT_SSL_CAINFO",
	"NODE_EXTRA_CA_CERTS",
	"REQUESTS_CA_BUNDLE",
	"SSL_CERT_DIR",
	"SSL_CERT_FILE",
}

func networkSandboxEnv(home, tmpdir string) []string {
	env := sandboxEnv(home, tmpdir)
	for _, name := range networkEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func bashArgs(command string) []string {
	return []string{"bash", "-c", command}
}

func runExternalSandbox(
	ctx context.Context,
	live, tempDir, command string,
	capBytes int,
	track func(int),
) (env.ExecResult, error) {
	args := bashArgs(command)
	cmd := osexec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = live
	cmd.Env = networkSandboxEnv(tempDir, tempDir)
	return collect(ctx, cmd, capBytes, track)
}

func collect(ctx context.Context, cmd *osexec.Cmd, capBytes int, track func(int)) (env.ExecResult, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return env.ExecResult{}, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return env.ExecResult{}, err
	}
	_ = pw.Close()
	pgid := cmd.Process.Pid
	if track != nil {
		track(pgid)
	}

	var buf capBuffer
	buf.cap = capBytes
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(&buf, pr)
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err = <-waited
	}
	drain := time.NewTimer(100 * time.Millisecond)
	select {
	case <-copied:
		drain.Stop()
	case <-drain.C:
		_ = pr.Close()
		<-copied
	}
	_ = pr.Close()
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
