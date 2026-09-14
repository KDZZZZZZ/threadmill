package exec

import (
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

const outputCopyBufferSize = 32 * 1024

var outputCopyBuffers = sync.Pool{New: func() any {
	buffer := make([]byte, outputCopyBufferSize)
	return &buffer
}}

type plainReader struct {
	io.Reader
}

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

var forwardedEnvironment = [...]string{
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
	"JAVA_HOME",
	"JAVA_TOOL_OPTIONS",
	"JDK_JAVA_OPTIONS",
	"GRADLE_OPTS",
	"MAVEN_OPTS",
	"MAVEN_ARGS",
	"GOPROXY",
	"GOSUMDB",
	"GOPRIVATE",
	"GONOPROXY",
	"GONOSUMDB",
	"GOFLAGS",
	"CGO_ENABLED",
	"CC",
	"CXX",
	"CFLAGS",
	"CXXFLAGS",
	"LDFLAGS",
	"PKG_CONFIG_PATH",
	"PYTHONPATH",
	"VIRTUAL_ENV",
}

func networkSandboxEnv(home, tmpdir string) []string {
	env := sandboxEnv(home, tmpdir)
	for _, name := range forwardedEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	if os.Getenv("GRADLE_USER_HOME") != "" {
		env = append(env, "GRADLE_USER_HOME="+filepath.Join(home, ".gradle"))
	}
	return env
}

// Keep the image's offline Gradle configuration and cache available without
// sharing a writable user home between tasks. cloning requires reflinks.
func seedGradleHome(ctx context.Context, home string) error {
	source := os.Getenv("GRADLE_USER_HOME")
	if source == "" {
		return nil
	}
	if _, err := os.Stat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	dest := filepath.Join(home, ".gradle")
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	return osexec.CommandContext(ctx, "cp", "--reflink=always", "-a", "--", filepath.Clean(source)+"/.", dest).Run()
}

func bashArgs(command string) []string {
	return []string{"bash", "-c", command}
}

func runExternalSandbox(
	ctx context.Context,
	live, tempDir, command string,
	capBytes int,
	track func(int),
	trace *traceRun,
) (env.ExecResult, error) {
	args := trace.wrap(bashArgs(command))
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

	meter := newProcMeter()
	meterStop := make(chan struct{})
	peakCh := make(chan int64, 1)
	go func() { peakCh <- meter.run(pgid, meterStop) }()

	var buf capBuffer
	buf.cap = capBytes
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		buffer := outputCopyBuffers.Get().(*[]byte)
		defer func() {
			clear(*buffer)
			outputCopyBuffers.Put(buffer)
		}()
		// *os.File implements io.WriterTo, which would make CopyBuffer ignore
		// the pooled buffer. Hide that optional method while draining the pipe.
		_, _ = io.CopyBuffer(&buf, plainReader{Reader: pr}, *buffer)
	}()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err = <-waited
	}
	close(meterStop)
	peakRSS := uint64(<-peakCh)
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
		return env.ExecResult{Output: out, PeakRSSBytes: peakRSS}, ctx.Err()
	}
	if err == nil {
		return env.ExecResult{Output: out, PeakRSSBytes: peakRSS}, nil
	}
	var ee *osexec.ExitError
	if errors.As(err, &ee) {
		return env.ExecResult{ExitCode: ee.ExitCode(), Output: out, PeakRSSBytes: peakRSS}, nil
	}
	return env.ExecResult{Output: out, PeakRSSBytes: peakRSS}, err
}
