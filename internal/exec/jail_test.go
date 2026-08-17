package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestBashCwdStaysInsideLiveDir(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap or landlock sandbox on this host")
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
	if pwd != live && !strings.HasPrefix(pwd, live+string(os.PathSeparator)) && pwd != "/" {
		t.Fatalf("pwd = %q, want live dir %q (or sandbox root /)", pwd, live)
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

func TestSandboxHidesHostSecrets(t *testing.T) {
	s := New(Config{Slots: 1})
	if s.sandbox == sandboxNone {
		t.Skip("no bwrap or landlock sandbox on this host")
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
