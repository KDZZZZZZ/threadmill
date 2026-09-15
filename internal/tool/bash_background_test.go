package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

type fakeBackground struct {
	fakeExec
	started string
	waited  time.Duration
	killed  string
}

func (f *fakeBackground) Start(_ context.Context, spec env.Cmd) (env.BackgroundStatus, error) {
	f.started = spec.Command
	return env.BackgroundStatus{ID: "bg-1", Running: true}, nil
}

func (f *fakeBackground) Output(_ context.Context, id string, wait time.Duration) (env.BackgroundStatus, error) {
	f.waited = wait
	return env.BackgroundStatus{ID: id, ExitCode: 3, Output: "tail", Dropped: 5}, nil
}

func (f *fakeBackground) Kill(id string) (env.BackgroundStatus, error) {
	f.killed = id
	return env.BackgroundStatus{ID: id, Killed: true}, nil
}

func TestShellToolDefinitionsValidate(t *testing.T) {
	t.Parallel()

	for _, tool := range ShellTools() {
		if err := tool.Definition().Validate(); err != nil {
			t.Fatalf("%s: %v", tool.Definition().Name, err)
		}
	}
	if schema := string(Bash().Definition().InputSchema); !strings.Contains(schema, `"run_in_background"`) {
		t.Fatalf("bash schema lacks run_in_background: %s", schema)
	}
}

func TestBashRunInBackgroundStartsWithoutWaiting(t *testing.T) {
	t.Parallel()

	fake := &fakeBackground{}
	tools := BindEnv(env.Open("env-1", nil).WithExec(fake), ShellTools())
	out, err := executeNamed(t, tools, "bash", `{"command":"npm run dev","run_in_background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.started != "npm run dev" {
		t.Fatalf("started = %q, want npm run dev", fake.started)
	}
	if !strings.Contains(out.Content, "bg-1") || !strings.Contains(out.Content, BashOutputName) {
		t.Fatalf("content = %q, want id and bash_output hint", out.Content)
	}
}

func TestBashRunInBackgroundRequiresCapability(t *testing.T) {
	t.Parallel()

	tools := BindEnv(env.Open("env-1", nil).WithExec(fakeExec{}), ShellTools())
	_, err := executeNamed(t, tools, "bash", `{"command":"sleep 30","run_in_background":true}`)
	if err == nil || !strings.Contains(err.Error(), "background commands are unavailable") {
		t.Fatalf("error = %v, want unavailable background commands", err)
	}
}

func TestBashOutputClampsWaitAndFormatsStatus(t *testing.T) {
	t.Parallel()

	fake := &fakeBackground{}
	tools := BindEnv(env.Open("env-1", nil).WithExec(fake), ShellTools())
	out, err := executeNamed(t, tools, BashOutputName, `{"id":"bg-1","wait":900}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.waited != maxBackgroundWaitSeconds*time.Second {
		t.Fatalf("wait = %v, want clamp to %ds", fake.waited, maxBackgroundWaitSeconds)
	}
	for _, want := range []string{"status: exited 3", "[5 bytes dropped", "tail"} {
		if !strings.Contains(out.Content, want) {
			t.Fatalf("content = %q, want %q", out.Content, want)
		}
	}
}

func TestBashKillReportsKilled(t *testing.T) {
	t.Parallel()

	fake := &fakeBackground{}
	tools := BindEnv(env.Open("env-1", nil).WithExec(fake), ShellTools())
	out, err := executeNamed(t, tools, BashKillName, `{"id":"bg-1"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.killed != "bg-1" || !strings.Contains(out.Content, "status: killed") {
		t.Fatalf("killed = %q, content = %q", fake.killed, out.Content)
	}
}

func TestBackgroundToolsUnboundError(t *testing.T) {
	t.Parallel()

	for _, name := range []string{BashOutputName, BashKillName} {
		_, err := executeNamed(t, ShellTools(), name, `{"id":"bg-1"}`)
		if err == nil || !strings.Contains(err.Error(), "not bound to env") {
			t.Fatalf("%s unbound error = %v, want not bound to env", name, err)
		}
	}
}
