package agentruntime

import (
	"strings"
	"testing"
)

func TestAgentRoleValidAndParse(t *testing.T) {
	t.Parallel()

	valid := []AgentRole{
		AgentRoleTaskManager,
		AgentRoleCtxManager,
		AgentRolePlanner,
		AgentRoleExecutor,
		AgentRoleVerifier,
	}
	for _, role := range valid {
		role := role
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			if !role.Valid() {
				t.Fatalf("%q should be valid", role)
			}
			parsed, err := ParseAgentRole(string(role))
			if err != nil {
				t.Fatalf("ParseAgentRole() error = %v", err)
			}
			if parsed != role {
				t.Fatalf("ParseAgentRole() = %q, want %q", parsed, role)
			}
		})
	}

	if AgentRole("reviewer").Valid() {
		t.Fatal("unsupported role should be invalid")
	}
	if _, err := ParseAgentRole("reviewer"); err == nil {
		t.Fatal("ParseAgentRole() should reject an unsupported role")
	}
}

func TestAgentPhaseValidAndParse(t *testing.T) {
	t.Parallel()

	valid := []AgentPhase{AgentPhasePlan, AgentPhaseExecute, AgentPhaseVerify}
	for _, phase := range valid {
		phase := phase
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()
			if !phase.Valid() {
				t.Fatalf("%q should be valid", phase)
			}
			parsed, err := ParseAgentPhase(string(phase))
			if err != nil {
				t.Fatalf("ParseAgentPhase() error = %v", err)
			}
			if parsed != phase {
				t.Fatalf("ParseAgentPhase() = %q, want %q", parsed, phase)
			}
		})
	}

	if AgentPhase("review").Valid() {
		t.Fatal("unsupported phase should be invalid")
	}
	if _, err := ParseAgentPhase("review"); err == nil {
		t.Fatal("ParseAgentPhase() should reject an unsupported phase")
	}
}

func TestAgentRunParamsValidateMissingRequiredFields(t *testing.T) {
	t.Parallel()

	base := AgentRunParams{
		TaskID:       "task-1",
		AttemptID:    "attempt-1",
		InvocationID: "invocation-1",
		Role:         AgentRolePlanner,
		Phase:        AgentPhasePlan,
		Instruction:  "Create an implementation plan.",
	}
	tests := []struct {
		name   string
		mutate func(*AgentRunParams)
		want   string
	}{
		{name: "invocation id", mutate: func(p *AgentRunParams) { p.InvocationID = "" }, want: "invocation_id is required"},
		{name: "instruction", mutate: func(p *AgentRunParams) { p.Instruction = " " }, want: "instruction is required"},
		{name: "role", mutate: func(p *AgentRunParams) { p.Role = "reviewer" }, want: "invalid agent role"},
		{name: "phase", mutate: func(p *AgentRunParams) { p.Phase = "review" }, want: "invalid agent phase"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := base
			tt.mutate(&params)
			err := params.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAgentRunParamsValidateWorkerRequirements(t *testing.T) {
	t.Parallel()

	base := AgentRunParams{
		TaskID:       "task-1",
		AttemptID:    "attempt-1",
		InvocationID: "invocation-1",
		Role:         AgentRolePlanner,
		Phase:        AgentPhasePlan,
		Instruction:  "Perform the assigned work.",
		WorktreeID:   "worktree-1",
	}
	tests := []struct {
		name   string
		mutate func(*AgentRunParams)
		want   string
	}{
		{
			name: "planner without task id",
			mutate: func(p *AgentRunParams) {
				p.Role = AgentRolePlanner
				p.TaskID = " "
			},
			want: "task_id is required",
		},
		{
			name: "planner without attempt id",
			mutate: func(p *AgentRunParams) {
				p.Role = AgentRolePlanner
				p.AttemptID = " "
			},
			want: "attempt_id is required",
		},
		{
			name: "executor without task id",
			mutate: func(p *AgentRunParams) {
				p.Role = AgentRoleExecutor
				p.TaskID = " "
			},
			want: "task_id is required",
		},
		{
			name: "executor without attempt id",
			mutate: func(p *AgentRunParams) {
				p.Role = AgentRoleExecutor
				p.AttemptID = " "
			},
			want: "attempt_id is required",
		},
		{
			name: "executor without worktree id",
			mutate: func(p *AgentRunParams) {
				p.Role = AgentRoleExecutor
				p.WorktreeID = " "
			},
			want: "worktree_id is required",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := base
			tt.mutate(&params)
			err := params.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAgentRunParamsValidateRoleSpecificRequirements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params AgentRunParams
	}{
		{
			name: "verifier with task and attempt",
			params: AgentRunParams{
				TaskID:       "task-1",
				AttemptID:    "attempt-1",
				InvocationID: "invocation-verify-1",
				Role:         AgentRoleVerifier,
				Phase:        AgentPhaseVerify,
				Instruction:  "Verify the task result.",
			},
		},
		{
			name: "task manager without task and attempt",
			params: AgentRunParams{
				InvocationID: "invocation-task-manager-1",
				Role:         AgentRoleTaskManager,
				Phase:        AgentPhasePlan,
				Instruction:  "Turn the requirement into task graph changes.",
			},
		},
		{
			name: "context manager without task and attempt",
			params: AgentRunParams{
				InvocationID: "invocation-context-manager-1",
				Role:         AgentRoleCtxManager,
				Phase:        AgentPhasePlan,
				Instruction:  "Perform project-level context maintenance.",
			},
		},
		{
			name: "role and phase are not fixed pairs",
			params: AgentRunParams{
				TaskID:       "task-1",
				AttemptID:    "attempt-1",
				InvocationID: "invocation-replan-1",
				Role:         AgentRolePlanner,
				Phase:        AgentPhaseExecute,
				Instruction:  "Replan after an execution failure.",
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.params.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestAgentRunParamsValidatePlannerInvocation(t *testing.T) {
	t.Parallel()

	params := AgentRunParams{
		TaskID:        "task-1",
		AttemptID:     "attempt-1",
		InvocationID:  "invocation-plan-1",
		Role:          AgentRolePlanner,
		Phase:         AgentPhasePlan,
		Instruction:   "Plan the requested runtime change.",
		ContextPackID: "context-pack-1",
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentRunParamsValidateExecutorInvocation(t *testing.T) {
	t.Parallel()

	params := AgentRunParams{
		TaskID:        "task-1",
		AttemptID:     "attempt-1",
		InvocationID:  "invocation-execute-1",
		Role:          AgentRoleExecutor,
		Phase:         AgentPhaseExecute,
		Instruction:   "Implement the approved plan.",
		ContextPackID: "context-pack-1",
		WorktreeID:    "worktree-1",
	}
	if err := params.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
