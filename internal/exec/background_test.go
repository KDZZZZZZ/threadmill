package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func newBackgroundView(t *testing.T, slots int) (*Scheduler, env.BackgroundExec, env.ExecView) {
	t.Helper()
	s := New(Config{Slots: slots})
	if s.sandbox != sandboxBwrap {
		t.Skip("bwrap unavailable")
	}
	view := s.View("env-a", vfs.NewStore(t.TempDir()))
	t.Cleanup(func() {
		if err := s.Reap("env-a"); err != nil {
			t.Errorf("Reap: %v", err)
		}
	})
	return s, view.(env.BackgroundExec), view
}

func waitBackgroundOutput(t *testing.T, bg env.BackgroundExec, id, want string) {
	t.Helper()
	var seen strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := bg.Output(context.Background(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen.WriteString(st.Output)
		if strings.Contains(seen.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("output of %s never contained %q: %q", id, want, seen.String())
}

// countProcesses 统计 argv[0] 为 marker 的存活进程；僵尸进程的 cmdline 为空，不计入。
func countProcesses(marker string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if strings.HasPrefix(string(data), marker+"\x00") {
			n++
		}
	}
	return n
}

func waitProcessCount(t *testing.T, marker string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for countProcesses(marker) != want {
		if !time.Now().Before(deadline) {
			t.Fatalf("processes named %s = %d, want %d", marker, countProcesses(marker), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBackgroundCommandOutlivesCallWithoutHoldingSlot(t *testing.T) {
	t.Parallel()
	s, bg, view := newBackgroundView(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	st, err := bg.Start(ctx, env.Cmd{Command: "echo ready > marker; echo ready; exec sleep 30"})
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.ID == "" {
		t.Fatalf("Start = %#v, want a running command with an id", st)
	}
	waitBackgroundOutput(t, bg, st.ID, "ready")

	// 单槽位下前台命令仍能运行，并看到后台命令写进同一工作区的文件。
	res, err := view.Run(context.Background(), env.Cmd{Command: "cat marker"})
	if err != nil || strings.TrimSpace(res.Output) != "ready" {
		t.Fatalf("foreground Run = %#v, %v", res, err)
	}
	status, err := bg.Output(context.Background(), st.ID, 0)
	if err != nil || !status.Running {
		t.Fatalf("Output after canceling the starting call = %#v, %v; want still running", status, err)
	}
	if stats := s.Stats(); stats.BackgroundRunning != 1 || stats.BackgroundStarted != 1 {
		t.Fatalf("background stats = running %d started %d, want 1/1", stats.BackgroundRunning, stats.BackgroundStarted)
	}
}

func TestReapKillsBackgroundCommandAndDetachedDescendants(t *testing.T) {
	t.Parallel()
	s, bg, _ := newBackgroundView(t, 1)

	marker := fmt.Sprintf("tmbg%d", time.Now().UnixNano())
	st, err := bg.Start(context.Background(), env.Cmd{
		Command: "setsid bash -c 'exec -a " + marker + " sleep 300' & exec -a " + marker + " sleep 300",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitProcessCount(t, marker, 2)

	if err := s.Reap("env-a"); err != nil {
		t.Fatal(err)
	}
	waitProcessCount(t, marker, 0)
	if _, err := bg.Output(context.Background(), st.ID, 0); !errors.Is(err, ErrUnknownBackground) {
		t.Fatalf("Output after Reap error = %v, want ErrUnknownBackground", err)
	}
	if stats := s.Stats(); stats.BackgroundRunning != 0 || stats.TrackedProcessGroups != 0 {
		t.Fatalf("stats after Reap = running %d tracked %d, want 0/0", stats.BackgroundRunning, stats.TrackedProcessGroups)
	}
}

func TestBackgroundKillStopsCommand(t *testing.T) {
	t.Parallel()
	s, bg, _ := newBackgroundView(t, 1)

	st, err := bg.Start(context.Background(), env.Cmd{Command: "echo started; exec sleep 300"})
	if err != nil {
		t.Fatal(err)
	}
	waitBackgroundOutput(t, bg, st.ID, "started")
	final, err := bg.Kill(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Running || !final.Killed {
		t.Fatalf("Kill = %#v, want killed and not running", final)
	}
	if _, err := bg.Output(context.Background(), st.ID, 0); !errors.Is(err, ErrUnknownBackground) {
		t.Fatalf("Output after Kill error = %v, want ErrUnknownBackground", err)
	}
	if got := s.Stats().BackgroundRunning; got != 0 {
		t.Fatalf("running after Kill = %d, want 0", got)
	}
}

func TestBackgroundOutputWaitReportsExit(t *testing.T) {
	t.Parallel()
	_, bg, _ := newBackgroundView(t, 1)

	st, err := bg.Start(context.Background(), env.Cmd{Command: "echo done; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := bg.Output(context.Background(), st.ID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Running || got.Killed || got.ExitCode != 3 {
		t.Fatalf("Output after exit = %#v, want exited 3", got)
	}
	if !strings.Contains(st.Output+got.Output, "done") {
		t.Fatalf("output = %q, want done", st.Output+got.Output)
	}
}

func TestBackgroundRunningLimit(t *testing.T) {
	t.Parallel()
	_, bg, _ := newBackgroundView(t, 1)

	for range maxBackgroundRunning {
		if _, err := bg.Start(context.Background(), env.Cmd{Command: "exec sleep 30"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := bg.Start(context.Background(), env.Cmd{Command: "exec sleep 30"}); !errors.Is(err, ErrBackgroundLimit) {
		t.Fatalf("Start beyond limit error = %v, want ErrBackgroundLimit", err)
	}
}

func TestBackgroundRequiresBwrap(t *testing.T) {
	t.Parallel()

	s := New(Config{Slots: 1, ExternalSandbox: true})
	bg := s.View("env-a", vfs.NewStore(t.TempDir())).(env.BackgroundExec)
	if _, err := bg.Start(context.Background(), env.Cmd{Command: "sleep 30"}); !errors.Is(err, ErrBackgroundUnsupported) {
		t.Fatalf("Start without bwrap error = %v, want ErrBackgroundUnsupported", err)
	}
}

func TestTailBufferKeepsNewestBytes(t *testing.T) {
	t.Parallel()

	b := tailBuffer{cap: 4}
	steps := []struct {
		write   string
		out     string
		dropped int64
	}{
		{"ab", "ab", 0},
		{"cdef", "cdef", 0},
		{"gh", "gh", 0},
		{"ijklmn", "klmn", 2},
	}
	for _, step := range steps {
		_, _ = b.Write([]byte(step.write))
		out, dropped := b.unread()
		if out != step.out || dropped != step.dropped {
			t.Fatalf("after %q unread = %q, %d; want %q, %d", step.write, out, dropped, step.out, step.dropped)
		}
	}
}
