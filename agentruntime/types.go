// Package agentruntime defines the stable inputs used to invoke agents.
package agentruntime

import (
	"fmt"
	"strings"
)

// AgentRole identifies the responsibility assigned to an agent invocation.
type AgentRole string

const (
	AgentRoleTaskManager AgentRole = "task_manager"
	AgentRoleCtxManager  AgentRole = "ctx_manager"
	AgentRolePlanner     AgentRole = "planner"
	AgentRoleExecutor    AgentRole = "executor"
	AgentRoleVerifier    AgentRole = "verifier"
)

// Valid reports whether r is a supported agent role.
func (r AgentRole) Valid() bool {
	switch r {
	case AgentRoleTaskManager, AgentRoleCtxManager, AgentRolePlanner, AgentRoleExecutor, AgentRoleVerifier:
		return true
	default:
		return false
	}
}

// ParseAgentRole parses a supported agent role.
func ParseAgentRole(value string) (AgentRole, error) {
	role := AgentRole(value)
	if !role.Valid() {
		return "", fmt.Errorf("invalid agent role %q", value)
	}
	return role, nil
}

// AgentPhase identifies the task phase in which an agent is invoked.
type AgentPhase string

const (
	AgentPhasePlan    AgentPhase = "plan"
	AgentPhaseExecute AgentPhase = "execute"
	AgentPhaseVerify  AgentPhase = "verify"
)

// Valid reports whether p is a supported agent phase.
func (p AgentPhase) Valid() bool {
	switch p {
	case AgentPhasePlan, AgentPhaseExecute, AgentPhaseVerify:
		return true
	default:
		return false
	}
}

// ParseAgentPhase parses a supported agent phase.
func ParseAgentPhase(value string) (AgentPhase, error) {
	phase := AgentPhase(value)
	if !phase.Valid() {
		return "", fmt.Errorf("invalid agent phase %q", value)
	}
	return phase, nil
}

// AgentRunParams describes one agent invocation requested by the control plane.
//
// ContextPackID and WorktreeID are references resolved by the runtime. The
// referenced objects and all invocation output remain outside these parameters.
type AgentRunParams struct {
	TaskID        string     `json:"task_id,omitempty"`
	AttemptID     string     `json:"attempt_id,omitempty"`
	InvocationID  string     `json:"invocation_id"`
	Role          AgentRole  `json:"role"`
	Phase         AgentPhase `json:"phase"`
	Instruction   string     `json:"instruction"`
	ContextPackID string     `json:"context_pack_id,omitempty"`
	WorktreeID    string     `json:"worktree_id,omitempty"`
}

// Validate checks the constraints required to start an agent invocation.
func (p AgentRunParams) Validate() error {
	required := []struct {
		name  string
		value string
	}{
		{name: "invocation_id", value: p.InvocationID},
		{name: "instruction", value: p.Instruction},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if !p.Role.Valid() {
		return fmt.Errorf("invalid agent role %q", p.Role)
	}
	if !p.Phase.Valid() {
		return fmt.Errorf("invalid agent phase %q", p.Phase)
	}
	switch p.Role {
	case AgentRolePlanner, AgentRoleExecutor, AgentRoleVerifier:
		if strings.TrimSpace(p.TaskID) == "" {
			return fmt.Errorf("task_id is required for agent role %q", p.Role)
		}
		if strings.TrimSpace(p.AttemptID) == "" {
			return fmt.Errorf("attempt_id is required for agent role %q", p.Role)
		}
	}
	if p.Role == AgentRoleExecutor && strings.TrimSpace(p.WorktreeID) == "" {
		return fmt.Errorf("worktree_id is required for agent role %q", p.Role)
	}
	return nil
}
