//go:build !linux

package exec

import (
	"context"
	"fmt"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func runExternalWorkspaceSandbox(
	context.Context,
	string, string, string, string,
	int,
	func(int),
	*traceRun,
) (env.ExecResult, error) {
	return env.ExecResult{}, fmt.Errorf(
		"%w: mount namespaces require Linux",
		ErrWorkspaceIsolationUnavailable,
	)
}

func probeExternalWorkspaceIsolation() bool { return false }
