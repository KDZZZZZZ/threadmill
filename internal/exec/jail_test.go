package exec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestBashCwdStaysInsideLiveDir(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap sandbox on this host")
	}

	files := vfs.NewStore(t.TempDir())
	view := s.View("env-a", files)
	res, err := view.Run(context.Background(), env.Cmd{Command: "pwd"})
	if err != nil {
		t.Fatal(err)
	}
	live, err := files.Materialize("env-a")
	if err != nil {
		t.Fatal(err)
	}
	pwd := strings.TrimSpace(res.Output)
	if pwd != live && !strings.HasPrefix(pwd, live+string(os.PathSeparator)) && pwd != bwrapWorkspace {
		t.Fatalf("pwd = %q, want live dir %q (or sandbox workspace %s)", pwd, live, bwrapWorkspace)
	}

	outside := filepath.Join(t.TempDir(), "escape.txt")
	escaped, err := view.Run(context.Background(), env.Cmd{
		Command: "cd / && echo escaped > " + outside,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Fatal("command wrote outside the live dir")
	}
	_ = escaped
}

func TestBwrapKeepsProjectBelowFilesystemRoot(t *testing.T) {
	if !probeBwrap() {
		t.Skip("bwrap unavailable")
	}
	if _, err := osexec.LookPath("go"); err != nil {
		t.Skip("go unavailable")
	}

	base := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(base, "go.mod"),
		[]byte("module example.com/threadmill/sandbox\n\ngo 1.22\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(base, "hello.go"),
		[]byte("package hello\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	s := New(Config{Slots: 1})
	files := vfs.NewStore(base)
	result, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `test "$PWD" = /workspace && test ! -e ./proc/tty/driver && go test ./...`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("sandbox project-root check = %#v, want exit 0", result)
	}
}

func TestSandboxHidesHostSecrets(t *testing.T) {
	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap sandbox on this host")
	}

	t.Setenv("OPENCODE_API_KEY", "secret-from-host")
	files := vfs.NewStore(t.TempDir())
	res, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: "printenv OPENCODE_API_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "secret-from-host") {
		t.Fatalf("sandbox leaked host secret: %q", res.Output)
	}
}

func TestSandboxReapsBackgroundJobs(t *testing.T) {
	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap sandbox on this host")
	}

	files := vfs.NewStore(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := s.View("env-a", files).Run(ctx, env.Cmd{Command: "sleep 30 &"}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("background sleep held the exec slot")
	}
	if err := s.Reap("env-a"); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxHasWritableTmp(t *testing.T) {
	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap sandbox on this host")
	}

	files := vfs.NewStore(t.TempDir())
	res, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `f=$(mktemp) && echo ok > "$f" && cat "$f"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "ok") {
		t.Fatalf("mktemp output = %q, want ok", res.Output)
	}
}

func TestBwrapSharesHostNetwork(t *testing.T) {
	if !probeBwrap() {
		t.Skip("bwrap unavailable")
	}
	if _, err := osexec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("reachable"))
	}))
	defer server.Close()
	s := New(Config{Slots: 1})
	files := vfs.NewStore(t.TempDir())
	result, err := s.View("env-a", files).Run(context.Background(), env.Cmd{
		Command: `curl --noproxy '*' --fail --silent --max-time 5 ` + server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Output != "reachable" {
		t.Fatalf("Run() = %#v, want host network response", result)
	}
}
