package manager

import (
	"context"
	"time"

	"github.com/KDZZZZZZ/threadmill/internal/cmdcache"
	tmexec "github.com/KDZZZZZZ/threadmill/internal/exec"
	"github.com/KDZZZZZZ/threadmill/internal/provider"
)

// ExecutionDiagnostics is installation evidence, independent of model credentials.
type ExecutionDiagnostics struct {
	DependencyTracing  bool   `json:"exec_dependency_tracing"`
	TracingEnabled     bool   `json:"exec_dependency_tracing_enabled"`
	Reason             string `json:"exec_dependency_tracing_reason"`
	Backend            string `json:"exec_sandbox_backend"`
	WorkspaceIsolation string `json:"exec_workspace_isolation"`
}

// ProbeExecution delegates a disposable startup check to the execution layer.
// It creates no Manager, task graph, project state, or provider client.
func ProbeExecution(ctx context.Context, opt Options) (ExecutionDiagnostics, error) {
	file, err := provider.LoadRuntimeConfig(opt.Root, opt.ConfigPath)
	if err != nil {
		return ExecutionDiagnostics{}, err
	}
	config := executionConfig(file, nil)
	config.DependencyTracing = file.Exec.Cache.Enabled
	scheduler := tmexec.New(config)
	status := scheduler.Stats()
	ok, reason := status.DependencyTracing, status.DependencyTracingReason
	if !ok {
		ok, reason = scheduler.ProbeDependencyTracing(ctx)
	}
	return ExecutionDiagnostics{DependencyTracing: ok, TracingEnabled: status.DependencyTracing, Reason: reason, Backend: status.SandboxBackend, WorkspaceIsolation: status.WorkspaceIsolation}, nil
}

func executionConfig(file provider.FileConfig, cache *cmdcache.Cache) tmexec.Config {
	return tmexec.Config{
		Slots:                      file.Exec.Slots,
		Timeout:                    time.Duration(file.Exec.Timeout) * time.Second,
		OutputCapKB:                file.Exec.OutputCapKB,
		ContainerImage:             file.Exec.ContainerImage,
		ExternalSandbox:            file.Exec.ExternalSandbox,
		ExternalWorkspaceIsolation: file.Exec.ExternalWorkspaceIsolation,
		Cache:                      cache,
		DisableTrace:               file.Exec.Cache.DisableTrace,
		HeavySlots:                 file.Exec.HeavySlots,
		HeavyThreshold:             time.Duration(file.Exec.HeavyThreshold) * time.Second,
		MemoryBudgetBytes:          file.Exec.MemoryBudgetMB << 20,
	}
}
