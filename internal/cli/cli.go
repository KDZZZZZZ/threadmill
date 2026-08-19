package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/session"
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

// Run 解析参数，打开会话，然后 REPL 或单发。
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
	in := stdio.In
	if in == nil {
		in = os.Stdin
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

	sess, err := open(ctx, session.Options{
		Root: opts.dir,
		Output: func(text string) {
			fmt.Fprintln(out, text)
		},
		OnEvent: func(_ context.Context, ev event.RuntimeEvent) {
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

	if opts.message != "" {
		sess.Send(opts.message)
		if err := sess.WaitIdle(ctx); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		return 0
	}
	return repl(ctx, sess, in, out, errOut)
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

func repl(ctx context.Context, sess *session.Session, in io.Reader, out, errOut io.Writer) int {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return 0
		}
		fmt.Fprint(out, "> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				fmt.Fprintln(errOut, err)
				return 1
			}
			return 0
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/quit" {
			return 0
		}
		sess.Send(line)
		if err := sess.WaitIdle(ctx); err != nil {
			fmt.Fprintln(errOut, err)
			if ctx.Err() != nil {
				return 0
			}
			return 1
		}
	}
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
