//go:build linux

package exec

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
	"unsafe"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

// go-landlock 需要 Go 1.24；这里用 stdlib syscall 调 Landlock。
const (
	landlockChildArg = "--threadmill-landlock"
	landlockChildEnv = "THREADMILL_LANDLOCK_CHILD"

	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1
	landlockRulePathBeneath      = 1
	prSetNoNewPrivs              = 0x26

	accessFSExecute    = 1 << 0
	accessFSWriteFile  = 1 << 1
	accessFSReadFile   = 1 << 2
	accessFSReadDir    = 1 << 3
	accessFSRemoveDir  = 1 << 4
	accessFSRemoveFile = 1 << 5
	accessFSMakeChar   = 1 << 6
	accessFSMakeDir    = 1 << 7
	accessFSMakeReg    = 1 << 8
	accessFSMakeSock   = 1 << 9
	accessFSMakeFIFO   = 1 << 10
	accessFSMakeBlock  = 1 << 11
	accessFSMakeSym    = 1 << 12
	accessFSRefer      = 1 << 13
	accessFSTruncate   = 1 << 14
)

func init() {
	if os.Getenv(landlockChildEnv) != "1" {
		return
	}
	if len(os.Args) >= 4 && os.Args[1] == landlockChildArg {
		os.Exit(runLandlockChild(os.Args[2], os.Args[3]))
	}
}

func probeLandlock() bool {
	abi, err := landlockABI()
	return err == nil && abi >= 1
}

func runLandlock(ctx context.Context, live, command string, capBytes int) (env.ExecResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return env.ExecResult{}, fmt.Errorf("exec: landlock: %w", err)
	}
	cmd := osexec.CommandContext(ctx, exe, landlockChildArg, live, command)
	cmd.Dir = live
	cmd.Env = append(os.Environ(), landlockChildEnv+"=1")
	return collect(ctx, cmd, capBytes)
}

func runLandlockChild(live, command string) int {
	bash, err := osexec.LookPath("bash")
	if err != nil {
		bash = "/bin/bash"
	}
	if err := restrictTo(live); err != nil {
		fmt.Fprintf(os.Stderr, "exec: landlock: %v\n", err)
		return 127
	}
	if err := os.Chdir(live); err != nil {
		fmt.Fprintf(os.Stderr, "exec: chdir: %v\n", err)
		return 127
	}
	if err := syscall.Exec(bash, []string{"bash", "-c", command}, landlockExecEnv()); err != nil {
		fmt.Fprintf(os.Stderr, "exec: bash: %v\n", err)
		return 127
	}
	return 0
}

func landlockExecEnv() []string {
	const prefix = landlockChildEnv + "="
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func landlockABI() (int, error) {
	r, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

type landlockRulesetAttr struct {
	accessFS uint64
}

type landlockPathBeneathAttr struct {
	allowedAccess uint64
	parentFd      int32
}

func restrictTo(live string) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	handled := fsAccess(abi)
	fd, err := createRuleset(handled)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if err := addPath(fd, live, handled); err != nil {
		return fmt.Errorf("live dir: %w", err)
	}
	ro := uint64(accessFSExecute | accessFSReadFile | accessFSReadDir)
	for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/dev"} {
		if err := addPath(fd, p, ro); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return errno
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, uintptr(fd), 0, 0); errno != 0 {
		return errno
	}
	return nil
}

func fsAccess(abi int) uint64 {
	access := uint64(
		accessFSExecute | accessFSWriteFile | accessFSReadFile | accessFSReadDir |
			accessFSRemoveDir | accessFSRemoveFile | accessFSMakeChar | accessFSMakeDir |
			accessFSMakeReg | accessFSMakeSock | accessFSMakeFIFO | accessFSMakeBlock |
			accessFSMakeSym,
	)
	if abi >= 2 {
		access |= accessFSRefer
	}
	if abi >= 3 {
		access |= accessFSTruncate
	}
	return access
}

func createRuleset(handled uint64) (int, error) {
	attr := landlockRulesetAttr{accessFS: handled}
	r, _, errno := syscall.Syscall(
		sysLandlockCreateRuleset,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

func addPath(ruleset int, path string, access uint64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	attr := landlockPathBeneathAttr{
		allowedAccess: access,
		parentFd:      int32(f.Fd()),
	}
	_, _, errno := syscall.Syscall6(
		sysLandlockAddRule,
		uintptr(ruleset),
		landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&attr)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
