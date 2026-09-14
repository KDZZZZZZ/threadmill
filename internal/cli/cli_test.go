package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/manager"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/tui"
)

var errSkipOpen = errors.New("open skipped")

type recordingMessageSender struct {
	messages []tea.Msg
}

func (s *recordingMessageSender) Send(msg tea.Msg) {
	s.messages = append(s.messages, msg)
}

func TestTUIEventBridgePreservesStartupEventsInOrder(t *testing.T) {
	bridge := newTUIEventBridge()
	managerOptions := newTUIManagerOptions(options{}, bridge)
	managerOptions.Output("resumed output")
	managerOptions.OnEvent(context.Background(), event.TaskStart("task-7"))

	target := &recordingMessageSender{}
	bridge.bind(target)
	bridge.send(tui.OutputMsg{Text: "live output"})

	if len(target.messages) != 3 {
		t.Fatalf("messages = %#v, want 3", target.messages)
	}
	first, ok := target.messages[0].(tui.OutputMsg)
	if !ok || first.Text != "resumed output" {
		t.Fatalf("first = %#v", target.messages[0])
	}
	second, ok := target.messages[1].(event.RuntimeEvent)
	if !ok || second.AgentID != "task-7" {
		t.Fatalf("second = %#v", target.messages[1])
	}
	last, ok := target.messages[2].(tui.OutputMsg)
	if !ok || last.Text != "live output" {
		t.Fatalf("last = %#v", target.messages[2])
	}
}

func TestParsePrintAndDir(t *testing.T) {
	t.Parallel()

	opts, err := parse([]string{
		"-C", "/tmp/ws",
		"-config", "/tmp/threadmill.yaml",
		"-p", "hello",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.dir != "/tmp/ws" || opts.configPath != "/tmp/threadmill.yaml" || opts.message != "hello" {
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
	var configPath string
	code := Run([]string{
		"-C", "/tmp/proj",
		"-config", "/tmp/settings.yaml",
		"-p", "do it",
	}, IO{
		In:  strings.NewReader(""),
		Out: io.Discard,
		Err: io.Discard,
		Open: func(_ context.Context, opt manager.Options) (*manager.Manager, error) {
			root = opt.Root
			configPath = opt.ConfigPath
			return nil, errSkipOpen
		},
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if root != "/tmp/proj" {
		t.Fatalf("root = %q, want /tmp/proj", root)
	}
	if configPath != "/tmp/settings.yaml" {
		t.Fatalf("config path = %q, want /tmp/settings.yaml", configPath)
	}
}

func TestRunInteractiveOpensWorkspace(t *testing.T) {
	var root string
	code := Run([]string{"-C", "/tmp/tui", "-config", "/tmp/settings.yaml"}, IO{
		Out: io.Discard,
		Err: io.Discard,
		Open: func(_ context.Context, opt manager.Options) (*manager.Manager, error) {
			root = opt.Root
			return nil, errSkipOpen
		},
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if root != "/tmp/tui" {
		t.Fatalf("root = %q, want /tmp/tui", root)
	}
}

func TestRunInteractiveCompletesFirstTimeSetupBeforeOpen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	var errOut strings.Builder
	opens := 0

	code := Run([]string{"-C", root}, IO{
		In: strings.NewReader(strings.Join([]string{
			"https://api.openai.com/v1",
			"gpt-test",
			"200000",
			"personal",
		}, "\n") + "\n"),
		Out: io.Discard,
		Err: &errOut,
		ReadSecret: func() (string, error) {
			return "sk-super-secret", nil
		},
		Open: func(_ context.Context, opt manager.Options) (*manager.Manager, error) {
			opens++
			config, err := provider.LoadRuntimeConfig(opt.Root, opt.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if config.LLM.BaseURL != "https://api.openai.com/v1" ||
				config.LLM.Model != "gpt-test" ||
				config.LLM.ContextWindow != 200000 ||
				config.LLM.Credential != "personal" {
				t.Fatalf("LLM config = %#v", config.LLM)
			}
			return nil, errSkipOpen
		},
	})
	if code != 1 {
		t.Fatalf("exit = %d, want 1 from the test open", code)
	}
	if opens != 1 {
		t.Fatalf("open calls = %d, want 1", opens)
	}
	if strings.Contains(errOut.String(), "sk-super-secret") {
		t.Fatal("CLI output contains API key")
	}
	configPath := filepath.Join(home, provider.ConfigDirName, provider.UserConfigFileName)
	if !strings.Contains(errOut.String(), configPath) {
		t.Fatalf("CLI output = %q, want saved config path %q", errOut.String(), configPath)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatal(err)
	}
}

func TestProgressLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   event.RuntimeEvent
		want string
	}{
		{
			name: "tool start",
			ev: event.RuntimeEvent{
				AgentID: "task-1:executor", Kind: event.KindTool,
				Phase: event.PhaseStart, Name: "bash", CallID: "call-1",
			},
			want: "[task-1:executor] tool bash start call_id=call-1",
		},
		{
			name: "tool end",
			ev: event.RuntimeEvent{
				AgentID: "task-1:executor", Kind: event.KindTool,
				Phase: event.PhaseEnd, Name: "bash", CallID: "call-1",
				Duration: 1500 * time.Millisecond, IsError: true,
				Err: "edit target not found\ntry read first",
			},
			want: "[task-1:executor] tool bash end call_id=call-1 duration=1.5s status=error error=\"edit target not found\\ntry read first\"",
		},
		{
			name: "model start",
			ev: event.RuntimeEvent{
				AgentID: "task-1:planner", Kind: event.KindModel,
				Phase: event.PhaseStart, Messages: 3, Tools: 8,
			},
			want: "[task-1:planner] model start messages=3 tools=8",
		},
		{
			name: "model end",
			ev: event.RuntimeEvent{
				AgentID: "task-1:planner", Kind: event.KindModel,
				Phase: event.PhaseEnd, Name: "gpt", Duration: 2 * time.Minute,
				ToolCalls: 1, Tokens: 42, Retries: 2,
			},
			want: "[task-1:planner] model end name=gpt duration=2m0s tool_calls=1 tokens=42 retries=2 status=ok",
		},
		{
			name: "model retry",
			ev: event.RuntimeEvent{
				AgentID: "task-1:planner", Kind: event.KindModel,
				Phase: event.PhaseRetry, Retries: 2,
			},
			want: "[task-1:planner] model retry attempt=2",
		},
		{
			name: "model error",
			ev: event.RuntimeEvent{
				AgentID: "task-1:planner", Kind: event.KindModel,
				Phase: event.PhaseEnd, Duration: time.Second,
				Err: "stream reset",
			},
			want: "[task-1:planner] model end duration=1s tool_calls=0 tokens=0 retries=0 status=error error=\"stream reset\"",
		},
		{
			name: "model delta",
			ev:   event.RuntimeEvent{Kind: event.KindModel, Phase: event.PhaseDelta},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := progressLine(test.ev); got != test.want {
				t.Fatalf("progressLine() = %q, want %q", got, test.want)
			}
		})
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

func TestStdoutPrinterDoesNotTreatEmptyActivityAsText(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	p := &stdoutPrinter{out: &out}
	p.delta(event.ModelDelta("manager", ""))
	p.write("completed snapshot")
	if out.String() != "completed snapshot\n" {
		t.Fatalf("out = %q", out.String())
	}
}
