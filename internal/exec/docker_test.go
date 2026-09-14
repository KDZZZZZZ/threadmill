package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestDockerArgsConstrainTaskContainer(t *testing.T) {
	args := dockerArgs("/tmp/live", "golang:1.26.5-alpine", "threadmill-test", 1000, 1000, "go test ./...")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--pull=never",
		"--rm",
		"--network=none",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges=true",
		"--pids-limit=256",
		"--memory=1g",
		"--memory-swap=1g",
		"--user=1000:1000",
		"--volume=/tmp/live:/workspace:rw",
		"--workdir=/workspace",
		"--tmpfs=/tmp:rw,exec,nosuid,nodev,size=512m",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("docker args missing %q: %s", want, joined)
		}
	}
	if !slices.Equal(args[len(args)-4:], []string{"golang:1.26.5-alpine", "/bin/sh", "-c", "go test ./..."}) {
		t.Fatalf("command tail = %q", args[len(args)-4:])
	}
}

func TestDockerSandboxRunsOnlyInWorkspace(t *testing.T) {
	const image = "golang:1.26.5-alpine"
	if !probeDocker(image) {
		t.Skip("local Docker sandbox image is unavailable")
	}
	t.Setenv("THREADMILL_TEST_SECRET", "must-not-leak")
	outside := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	files := vfs.NewStore(t.TempDir())
	view := files.View("env-a")
	if err := view.Write("go.mod", []byte("module example.com/sandbox\n\ngo 1.26.0\n")); err != nil {
		t.Fatal(err)
	}
	if err := view.Write("sandbox_test.go", []byte("package sandbox\n\nimport \"testing\"\n\nfunc TestSandbox(t *testing.T) {}\n")); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Slots: 1, ContainerImage: image})
	s.sandbox = sandboxDocker
	s.image = image
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.View("env-a", files).Run(ctx, env.Cmd{Command: fmt.Sprintf(`
		test -z "$THREADMILL_TEST_SECRET" || exit 8
		grep -q 'eth0:' /proc/net/dev && exit 9
		test ! -e %q || exit 10
		cp /bin/busybox /tmp/true && /tmp/true || exit 11
		go test ./... || exit 12
		printf ok > result.txt
	`, outside)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, output = %q", result.ExitCode, result.Output)
	}
	if err := files.Release("env-a"); err != nil {
		t.Fatal(err)
	}
	got, err := files.View("env-a").Read("result.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("result.txt = %q", got)
	}
}
