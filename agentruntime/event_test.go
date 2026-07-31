package agentruntime

import (
	"strings"
	"testing"
	"time"
)

func TestAgentEventTypeValidAndParse(t *testing.T) {
	t.Parallel()

	valid := []AgentEventType{
		AgentEventInvocationStarted,
		AgentEventInvocationOutput,
		AgentEventArtifactCreated,
		AgentEventInvocationCompleted,
		AgentEventInvocationFailed,
		AgentEventInvocationCancelled,
	}
	for _, eventType := range valid {
		eventType := eventType
		t.Run(string(eventType), func(t *testing.T) {
			t.Parallel()
			if !eventType.Valid() {
				t.Fatalf("%q should be valid", eventType)
			}
			parsed, err := ParseAgentEventType(string(eventType))
			if err != nil {
				t.Fatalf("ParseAgentEventType() error = %v", err)
			}
			if parsed != eventType {
				t.Fatalf("ParseAgentEventType() = %q, want %q", parsed, eventType)
			}
		})
	}

	if AgentEventType("invocation_created").Valid() {
		t.Fatal("unsupported event type should be invalid")
	}
	if _, err := ParseAgentEventType("invocation_created"); err == nil {
		t.Fatal("ParseAgentEventType() should reject an unsupported event type")
	}
}

func TestAgentEventValidateRequiredFields(t *testing.T) {
	t.Parallel()

	base := validAgentEvent()
	tests := []struct {
		name   string
		mutate func(*AgentEvent)
		want   string
	}{
		{
			name:   "event id",
			mutate: func(event *AgentEvent) { event.EventID = " " },
			want:   "event_id is required",
		},
		{
			name:   "invocation id",
			mutate: func(event *AgentEvent) { event.InvocationID = " " },
			want:   "invocation_id is required",
		},
		{
			name:   "event type",
			mutate: func(event *AgentEvent) { event.Type = "invocation_created" },
			want:   "invalid agent event type",
		},
		{
			name:   "role",
			mutate: func(event *AgentEvent) { event.Role = "reviewer" },
			want:   "invalid agent role",
		},
		{
			name:   "phase",
			mutate: func(event *AgentEvent) { event.Phase = "review" },
			want:   "invalid agent phase",
		},
		{
			name:   "occurred at",
			mutate: func(event *AgentEvent) { event.OccurredAt = time.Time{} },
			want:   "occurred_at is required",
		},
		{
			name: "occurred at not UTC",
			mutate: func(event *AgentEvent) {
				event.OccurredAt = time.Date(2026, time.July, 31, 9, 2, 3, 0, time.FixedZone("UTC+8", 8*60*60))
			},
			want: "occurred_at must be in UTC",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event := base
			tt.mutate(&event)
			err := event.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAgentEventValidateWorkerRequirements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*AgentEvent)
		want   string
	}{
		{
			name: "planner without task id",
			mutate: func(event *AgentEvent) {
				event.Role = AgentRolePlanner
				event.TaskID = " "
			},
			want: "task_id is required",
		},
		{
			name: "executor without attempt id",
			mutate: func(event *AgentEvent) {
				event.Role = AgentRoleExecutor
				event.AttemptID = " "
			},
			want: "attempt_id is required",
		},
		{
			name: "verifier without task id",
			mutate: func(event *AgentEvent) {
				event.Role = AgentRoleVerifier
				event.TaskID = " "
			},
			want: "task_id is required",
		},
		{
			name: "verifier without attempt id",
			mutate: func(event *AgentEvent) {
				event.Role = AgentRoleVerifier
				event.AttemptID = " "
			},
			want: "attempt_id is required",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			event := validAgentEvent()
			tt.mutate(&event)
			err := event.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestAgentEventValidateSystemRolesWithoutTask(t *testing.T) {
	t.Parallel()

	for _, role := range []AgentRole{AgentRoleTaskManager, AgentRoleCtxManager} {
		role := role
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			event := validAgentEvent()
			event.Role = role
			event.TaskID = ""
			event.AttemptID = ""
			if err := event.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestAgentEventValidateInvocationStarted(t *testing.T) {
	t.Parallel()

	event := validAgentEvent()
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentEventValidateArtifactCreated(t *testing.T) {
	t.Parallel()

	event := validAgentEvent()
	event.Type = AgentEventArtifactCreated

	if err := event.Validate(); err == nil || !strings.Contains(err.Error(), "artifact_id is required") {
		t.Fatalf("Validate() error = %v, want missing artifact_id error", err)
	}

	event.ArtifactID = "artifact-1"
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentEventValidateInvocationFailed(t *testing.T) {
	t.Parallel()

	event := validAgentEvent()
	event.Type = AgentEventInvocationFailed

	if err := event.Validate(); err == nil || !strings.Contains(err.Error(), "error_message is required") {
		t.Fatalf("Validate() error = %v, want missing error_message error", err)
	}

	event.ErrorMessage = "provider process exited unexpectedly"
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentEventValidateInvocationCompletedWithoutResult(t *testing.T) {
	t.Parallel()

	event := validAgentEvent()
	event.Type = AgentEventInvocationCompleted
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAgentEventValidateDoesNotFixRolePhasePairs(t *testing.T) {
	t.Parallel()

	event := validAgentEvent()
	event.Role = AgentRolePlanner
	event.Phase = AgentPhaseExecute
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func validAgentEvent() AgentEvent {
	return AgentEvent{
		EventID:      "event-1",
		TaskID:       "task-1",
		AttemptID:    "attempt-1",
		InvocationID: "invocation-1",
		Type:         AgentEventInvocationStarted,
		Role:         AgentRoleExecutor,
		Phase:        AgentPhaseExecute,
		OccurredAt:   time.Date(2026, time.July, 31, 1, 2, 3, 0, time.UTC),
	}
}
