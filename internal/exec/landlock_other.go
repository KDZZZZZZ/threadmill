//go:build !linux

package exec

import (
	"context"

	"github.com/KDZZZZZZ/threadmill/internal/env"
)

func probeLandlock() bool { return false }

func runLandlock(context.Context, string, string, int) (env.ExecResult, error) {
	return env.ExecResult{}, ErrSandboxUnavailable
}
