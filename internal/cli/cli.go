package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"

	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/manager"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
	"github.com/KDZZZZZZ/threadmill/internal/tui"
)

// IO 是 CLI 的输入输出和可选的 manager 工厂。
type IO struct {
	In         io.Reader
	Out        io.Writer
	Err        io.Writer
	Open       func(context.Context, manager.Options) (*manager.Manager, error)
	ReadSecret func() (string, error)
}

type options struct {
	dir        string
	configPath string
	message    string
}

// Run 解析参数：-p 单发纯文本，默认进入 TUI。
func Run(args []string, stdio IO) int {
	errOut := stdio.Err
	if errOut == nil {
		errOut = os.Stderr
	}
	opts, err := parse(args, errOut)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintln(errOut, err)
		return 2
	}
	open := stdio.Open
	if open == nil {
		open = manager.Open
	}
	out := stdio.Out
	if out == nil {
		out = os.Stdout
	}
	in := stdio.In
	if in == nil {
		in = os.Stdin
	}
	stdio.In = in
	stdio.Err = errOut

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	if opts.message != "" {
		return runPrint(ctx, stop, opts, open, out, errOut)
	}
	return runTUI(ctx, opts, open, stdio)
}

func runPrint(ctx context.Context, stop context.CancelFunc, opts options, open func(context.Context, manager.Options) (*manager.Manager, error), out, errOut io.Writer) int {
	printer := &stdoutPrinter{out: out}
	mgr, err := open(ctx, manager.Options{
		Root:       opts.dir,
		ConfigPath: opts.configPath,
		Output:     printer.write,
		OnEvent: func(_ context.Context, ev event.RuntimeEvent) {
			printer.delta(ev)
			if line := progressLine(ev); line != "" {
				fmt.Fprintln(errOut, line)
			}
		},
	})
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer mgr.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if !mgr.Cancel() {
					stop()
					return
				}
			}
		}
	}()

	mgr.Send(opts.message)
	if err := mgr.WaitIdle(ctx); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func runTUI(
	ctx context.Context,
	opts options,
	open func(context.Context, manager.Options) (*manager.Manager, error),
	stdio IO,
) int {
	needsSetup, err := provider.NeedsSetup(opts.dir, opts.configPath)
	if err != nil {
		fmt.Fprintln(stdio.Err, err)
		return 1
	}
	setupComplete := false
	if needsSetup {
		if err := firstTimeSetup(opts, stdio.In, stdio.Err, stdio.ReadSecret); err != nil {
			fmt.Fprintln(stdio.Err, err)
			return 1
		}
		setupComplete = true
	}
	var p *tea.Program
	managerOptions := manager.Options{
		Root:       opts.dir,
		ConfigPath: opts.configPath,
		Output: func(text string) {
			if p != nil {
				p.Send(tui.OutputMsg{Text: text})
			}
		},
		OnEvent: func(_ context.Context, ev event.RuntimeEvent) {
			if p != nil {
				p.Send(ev)
			}
		},
	}
	mgr, err := open(ctx, managerOptions)
	if !setupComplete && errors.Is(err, provider.ErrCredentialNotFound) {
		if err := firstTimeSetup(opts, stdio.In, stdio.Err, stdio.ReadSecret); err != nil {
			fmt.Fprintln(stdio.Err, err)
			return 1
		}
		mgr, err = open(ctx, managerOptions)
	}
	if err != nil {
		fmt.Fprintln(stdio.Err, err)
		return 1
	}
	defer mgr.Close()

	p = tui.NewProgram(ctx, mgr, tui.Info{Root: opts.dir, Model: mgr.ModelName()})
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(stdio.Err, err)
		return 1
	}
	return 0
}

func firstTimeSetup(
	opts options,
	in io.Reader,
	out io.Writer,
	readSecret func() (string, error),
) error {
	config, err := provider.LoadRuntimeConfig(opts.dir, opts.configPath)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	fmt.Fprintln(out, "Threadmill first-time setup")
	fmt.Fprintf(out, "Provider: %s\n", provider.OpenAIResponses)
	baseURL, err := prompt(reader, out, "Base URL", config.LLM.BaseURL)
	if err != nil {
		return err
	}
	model, err := prompt(reader, out, "Model", config.LLM.Model)
	if err != nil {
		return err
	}
	contextText, err := prompt(
		reader,
		out,
		"Context window",
		strconv.Itoa(config.LLM.ContextWindow),
	)
	if err != nil {
		return err
	}
	contextWindow, err := strconv.Atoi(contextText)
	if err != nil || contextWindow < 0 {
		return fmt.Errorf("context window must be a non-negative integer")
	}
	credential, err := prompt(reader, out, "Credential name", config.LLM.Credential)
	if err != nil {
		return err
	}
	if readSecret == nil {
		readSecret = func() (string, error) {
			return terminalSecret(in)
		}
	}
	fmt.Fprint(out, "API key: ")
	apiKey, err := readSecret()
	fmt.Fprintln(out)
	if err != nil {
		return err
	}
	path, err := provider.SaveUserSetup(provider.LLMConfig{
		Provider:      provider.OpenAIResponses,
		BaseURL:       baseURL,
		Credential:    credential,
		Model:         model,
		ContextWindow: contextWindow,
	}, apiKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Configuration saved to %s\n", path)
	return nil
}

func prompt(reader *bufio.Reader, out io.Writer, label, defaultValue string) (string, error) {
	fmt.Fprintf(out, "%s [%s]: ", label, defaultValue)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultValue
	}
	return value, nil
}

func terminalSecret(in io.Reader) (string, error) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return "", errors.New("API key input requires an interactive terminal")
	}
	value, err := term.ReadPassword(file.Fd())
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	return string(value), nil
}

func parse(args []string, errOut io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("threadmill", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&opts.dir, "C", "", "workspace directory (default: cwd)")
	fs.StringVar(
		&opts.configPath,
		"config",
		"",
		"highest-priority configuration override file",
	)
	fs.StringVar(&opts.message, "p", "", "send one message and exit")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if opts.dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return options{}, err
		}
		opts.dir = wd
	}
	return opts, nil
}

func progressLine(ev event.RuntimeEvent) string {
	if ev.Phase == event.PhaseDelta {
		return ""
	}
	prefix := ""
	if ev.AgentID != "" {
		prefix = fmt.Sprintf("[%s] ", ev.AgentID)
	}
	switch ev.Kind {
	case event.KindTool:
		line := fmt.Sprintf("%stool %s %s", prefix, ev.Name, ev.Phase)
		if ev.CallID != "" {
			line += " call_id=" + ev.CallID
		}
		if ev.Phase == event.PhaseEnd {
			line += " duration=" + ev.Duration.String()
			if ev.IsError || ev.Err != "" {
				line += " status=error"
				if ev.Err != "" {
					line += fmt.Sprintf(" error=%q", ev.Err)
				}
			} else {
				line += " status=ok"
			}
		}
		return line
	case event.KindModel:
		line := fmt.Sprintf("%smodel %s", prefix, ev.Phase)
		if ev.Phase == event.PhaseStart {
			return fmt.Sprintf("%s messages=%d tools=%d", line, ev.Messages, ev.Tools)
		}
		if ev.Phase == event.PhaseRetry {
			return fmt.Sprintf("%s attempt=%d", line, ev.Retries)
		}
		if ev.Phase != event.PhaseEnd {
			return ""
		}
		if ev.Name != "" {
			line += " name=" + ev.Name
		}
		line += fmt.Sprintf(
			" duration=%s tool_calls=%d tokens=%d retries=%d",
			ev.Duration,
			ev.ToolCalls,
			ev.Tokens,
			ev.Retries,
		)
		if ev.Err != "" {
			line += " status=error"
			line += fmt.Sprintf(" error=%q", ev.Err)
		} else {
			line += " status=ok"
		}
		return line
	default:
		return ""
	}
}

// stdoutPrinter 把经理的 SSE 增量打到 stdout；任务报告和未流式的整段回复仍走 write。
type stdoutPrinter struct {
	out      io.Writer
	mu       sync.Mutex
	streamed bool
}

func (p *stdoutPrinter) write(text string) {
	if p == nil || p.out == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.HasPrefix(text, "[任务报告]") {
		fmt.Fprintln(p.out, text)
		return
	}
	if p.streamed {
		p.streamed = false
		fmt.Fprintln(p.out)
		return
	}
	fmt.Fprintln(p.out, text)
}

func (p *stdoutPrinter) delta(ev event.RuntimeEvent) {
	if p == nil || p.out == nil {
		return
	}
	if ev.Kind != event.KindModel || ev.Phase != event.PhaseDelta || ev.AgentID != "manager" {
		return
	}
	if ev.Delta == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamed = true
	fmt.Fprint(p.out, ev.Delta)
}
