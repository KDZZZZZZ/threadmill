package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/session"
)

var errSkipOpen = errors.New("open skipped")

func TestParsePrintAndDir(t *testing.T) {
	t.Parallel()

	opts, err := parse([]string{"-C", "/tmp/ws", "-p", "hello"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.dir != "/tmp/ws" || opts.message != "hello" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseHelp(t *testing.T) {
	t.Parallel()

	_, err := parse([]string{"-h"}, io.Discard)
	if err == nil {
		t.Fatal("want help error")
	}
}

func TestRunPrintOpensWorkspace(t *testing.T) {
	var root string
	code := Run([]string{"-C", "/tmp/proj", "-p", "do it"}, IO{
		In:  strings.NewReader(""),
		Out: io.Discard,
		Err: io.Discard,
		Open: func(_ context.Context, opt session.Options) (*session.Session, error) {
			root = opt.Root
			return nil, errSkipOpen
		},
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if root != "/tmp/proj" {
		t.Fatalf("root = %q, want /tmp/proj", root)
	}
}

func TestProgressLine(t *testing.T) {
	t.Parallel()

	got := progressLine(event.RuntimeEvent{
		AgentID: "task-1:executor",
		Kind:    event.KindTool,
		Phase:   event.PhaseStart,
		Name:    "bash",
	})
	if got != "[task-1:executor] tool bash start" {
		t.Fatalf("line = %q", got)
	}
	if progressLine(event.RuntimeEvent{Kind: event.KindModel, Phase: event.PhaseStart}) != "" {
		t.Fatal("model events should not print progress")
	}
}

func TestStdoutPrinterStreamsManagerThenSkipsFullText(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	p := &stdoutPrinter{out: &out}
	p.delta(event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "Hel",
	})
	p.delta(event.RuntimeEvent{
		AgentID: "task-1:executor",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "nope",
	})
	p.delta(event.RuntimeEvent{
		AgentID: "manager",
		Kind:    event.KindModel,
		Phase:   event.PhaseDelta,
		Delta:   "lo",
	})
	p.write("Hello")
	p.write("[任务报告] task-1 · done · 耗时 1s\n目标: x\nverifier 输出:\nok")
	got := out.String()
	want := "Hello\n[任务报告] task-1 · done · 耗时 1s\n目标: x\nverifier 输出:\nok\n"
	if got != want {
		t.Fatalf("out = %q, want %q", got, want)
	}
}

func TestStdoutPrinterWritesWhenNotStreamed(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	p := &stdoutPrinter{out: &out}
	p.write("plain")
	if out.String() != "plain\n" {
		t.Fatalf("out = %q", out.String())
	}
}
