package exec

import (
	"context"
	osexec "os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCollectReportsZeroExitForTrue(t *testing.T) {
	t.Parallel()

	cmd := osexec.Command("bash", "-c", "true")
	res, err := collect(context.Background(), cmd, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestCollectLeavesBackgroundJobs(t *testing.T) {
	t.Parallel()

	cmd := osexec.Command("bash", "-c", "sleep 30 & echo started")
	start := time.Now()
	res, err := collect(context.Background(), cmd, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("background sleep held collect")
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "started") {
		t.Fatalf("output = %q, want started", res.Output)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("background job was killed: %v", err)
	}
}
