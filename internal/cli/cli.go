package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/session"
	"github.com/KDZZZZZZ/threadmill/internal/tui"
)

// IO 是 CLI 的输入输出和可选的会话工厂。
type IO struct {
	In   io.Reader
	Out  io.Writer
	Err  io.Writer
	Open func(context.Context, session.Options) (*session.Session, error)
}

type options struct {
	dir     string
	message string
}

// Run 解析参数：-p 单发纯文本，默认进入 TUI。
func Run(args []string, stdio IO) int {
	opts, err := parse(args, stdio.Err)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		fmt.Fprintln(stdio.Err, err)
		return 2
	}
	open := stdio.Open
	if open == nil {
		open = session.Open
	}
	out := stdio.Out
	if out == nil {
		out = os.Stdout
	}
	errOut := stdio.Err
	if errOut == nil {
		errOut = os.Stderr
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	if opts.message != "" {
		return runPrint(ctx, stop, opts, open, out, errOut)
	}
	return runTUI(ctx, opts, open, errOut)
}

func runPrint(ctx context.Context, stop context.CancelFunc, opts options, open func(context.Context, session.Options) (*session.Session, error), out, errOut io.Writer) int {
	printer := &stdoutPrinter{out: out}
	sess, err := open(ctx, session.Options{
		Root:   opts.dir,
		Output: printer.write,
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
	defer sess.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if !sess.Cancel() {
					stop()
					return
				}
			}
		}
	}()

	sess.Send(opts.message)
	if err := sess.WaitIdle(ctx); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func runTUI(ctx context.Context, opts options, open func(context.Context, session.Options) (*session.Session, error), errOut io.Writer) int {
	var p *tea.Program
	sess, err := open(ctx, session.Options{
		Root: opts.dir,
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
	})
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer sess.Close()

	p = tui.NewProgram(ctx, sess, tui.Info{Root: opts.dir, Model: sess.ModelName()})
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func parse(args []string, errOut io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet("threadmill", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&opts.dir, "C", "", "workspace directory (default: cwd)")
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
	if ev.Kind != event.KindTool {
		return ""
	}
	name := ev.Name
	if ev.AgentID != "" {
		return fmt.Sprintf("[%s] tool %s %s", ev.AgentID, name, ev.Phase)
	}
	return fmt.Sprintf("tool %s %s", name, ev.Phase)
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streamed = true
	fmt.Fprint(p.out, ev.Delta)
}
