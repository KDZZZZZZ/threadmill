package exec

import (
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

var nextDockerRunID atomic.Uint64

func probeDocker(image string) bool {
	if image == "" {
		return false
	}
	if _, err := osexec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "docker", "image", "inspect", image)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func runDocker(
	ctx context.Context,
	live, command, image string,
	capBytes int,
	track func(int),
) (env.ExecResult, error) {
	name := fmt.Sprintf("threadmill-%d-%d", os.Getpid(), nextDockerRunID.Add(1))
	cmd := osexec.CommandContext(
		ctx,
		"docker",
		dockerArgs(live, image, name, os.Getuid(), os.Getgid(), command)...,
	)
	result, err := collect(ctx, cmd, capBytes, track)
	if ctx.Err() != nil || err != nil {
		cleanupDockerContainer(name)
	}
	return result, err
}

func dockerArgs(live, image, name string, uid, gid int, command string) []string {
	return []string{
		"run",
		"--pull=never",
		"--rm",
		"--name=" + name,
		"--network=none",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges=true",
		"--pids-limit=256",
		"--memory=1g",
		"--memory-swap=1g",
		"--user=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		"--volume=" + live + ":/workspace:rw",
		"--workdir=/workspace",
		"--tmpfs=/tmp:rw,exec,nosuid,nodev,size=512m",
		"--env=HOME=/tmp",
		"--env=TMPDIR=/tmp",
		"--env=GOCACHE=/tmp/go-cache",
		"--env=LANG=C.UTF-8",
		image,
		"/bin/sh",
		"-c",
		command,
	}
}

func cleanupDockerContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "docker", "rm", "--force", name)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}
