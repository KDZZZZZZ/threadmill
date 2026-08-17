package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func TestBashDefinitionOmitsEnvAndCwd(t *testing.T) {
	t.Parallel()

	def := Bash().Definition()
	if def.Name != "bash" {
		t.Fatalf("name = %q, want bash", def.Name)
	}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	schema := string(def.InputSchema)
	if strings.Contains(schema, `"env"`) || strings.Contains(schema, `"cwd"`) {
		t.Fatalf("bash schema must not contain env or cwd: %s", schema)
	}
	if !strings.Contains(schema, `"command"`) {
		t.Fatalf("bash schema missing command: %s", schema)
	}
}

func TestBashUnboundExecuteError(t *testing.T) {
	t.Parallel()

	_, err := executeNamed(t, []Tool{Bash()}, "bash", `{"command":"true"}`)
	if err == nil || !strings.Contains(err.Error(), "not bound to env") {
		t.Fatalf("unbound Execute error = %v, want not bound to env", err)
	}
}

func TestBindEnvSwapsExecView(t *testing.T) {
	t.Parallel()

	one := fakeExec{output: "one"}
	two := fakeExec{output: "two"}

	tools := BindEnv(env.Open("env-1", nil).WithExec(one), []Tool{Bash()})
	out, err := executeNamed(t, tools, "bash", `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("bash env-1: %v", err)
	}
	if !strings.Contains(out.Content, "one") {
		t.Fatalf("bash env-1 = %q, want one", out.Content)
	}

	tools = BindEnv(env.Open("env-2", nil).WithExec(two), tools)
	out, err = executeNamed(t, tools, "bash", `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("bash env-2: %v", err)
	}
	if !strings.Contains(out.Content, "two") || strings.Contains(out.Content, "one") {
		t.Fatalf("BindEnv did not swap Exec: %q", out.Content)
	}
}

func TestBashNonZeroExitIsOutput(t *testing.T) {
	t.Parallel()

	tools := BindEnv(env.Open("env-1", nil).WithExec(fakeExec{
		exitCode: 2,
		output:   "nope",
	}), []Tool{Bash()})
	out, err := executeNamed(t, tools, "bash", `{"command":"false"}`)
	if err != nil {
		t.Fatalf("non-zero exit returned error %v, want Output", err)
	}
	if !strings.Contains(out.Content, "nope") {
		t.Fatalf("output = %q, want nope", out.Content)
	}
}

type fakeExec struct {
	exitCode int
	output   string
}

func (f fakeExec) Run(_ context.Context, _ env.Cmd) (env.ExecResult, error) {
	return env.ExecResult{ExitCode: f.exitCode, Output: f.output}, nil
}
