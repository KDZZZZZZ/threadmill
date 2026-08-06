package agentruntime

import (
	"fmt"
	"strings"
	"time"
)

// AgentEventType identifies a fact that occurred during an agent invocation.
type AgentEventType string

const (
	AgentEventInvocationStarted   AgentEventType = "invocation_started"
	AgentEventInvocationOutput    AgentEventType = "invocation_output"
	AgentEventArtifactCreated     AgentEventType = "artifact_created"
	AgentEventInvocationCompleted AgentEventType = "invocation_completed"
	AgentEventInvocationFailed    AgentEventType = "invocation_failed"
	AgentEventInvocationCancelled AgentEventType = "invocation_cancelled"
)

// Valid reports whether t is a supported agent event type.
func (t AgentEventType) Valid() bool {
	switch t {
	case AgentEventInvocationStarted,
		AgentEventInvocationOutput,
		AgentEventArtifactCreated,
		AgentEventInvocationCompleted,
		AgentEventInvocationFailed,
		AgentEventInvocationCancelled:
		return true
	default:
		return false
	}
}

// ParseAgentEventType parses a supported agent event type.
func ParseAgentEventType(value string) (AgentEventType, error) {
	eventType := AgentEventType(value)
	if !eventType.Valid() {
		return "", fmt.Errorf("invalid agent event type %q", value)
	}
	return eventType, nil
}

// AgentEvent describes a fact that occurred during one agent invocation.
//
// OccurredAt is supplied by the runtime in UTC. Message and ErrorMessage are
// limited text; large output is stored separately and referenced by ArtifactID.
type AgentEvent struct {
	EventID      string         `json:"event_id"`
	TaskID       string         `json:"task_id,omitempty"`
	AttemptID    string         `json:"attempt_id,omitempty"`
	InvocationID string         `json:"invocation_id"`
	Type         AgentEventType `json:"type"`
	Role         AgentRole      `json:"role"`
	Phase        AgentPhase     `json:"phase"`
	OccurredAt   time.Time      `json:"occurred_at"`
	Message      string         `json:"message,omitempty"`
	ArtifactID   string         `json:"artifact_id,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
}

// Validate checks the constraints required to record an agent event.
func (e AgentEvent) Validate() error {
	required := []struct {
		name  string
		value string
	}{
		{name: "event_id", value: e.EventID},
		{name: "invocation_id", value: e.InvocationID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if !e.Type.Valid() {
		return fmt.Errorf("invalid agent event type %q", e.Type)
	}
	if !e.Role.Valid() {
		return fmt.Errorf("invalid agent role %q", e.Role)
	}
	if !e.Phase.Valid() {
		return fmt.Errorf("invalid agent phase %q", e.Phase)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	if _, offset := e.OccurredAt.Zone(); offset != 0 {
		return fmt.Errorf("occurred_at must be in UTC")
	}
	switch e.Role {
	case AgentRolePlanner, AgentRoleExecutor, AgentRoleVerifier:
		if strings.TrimSpace(e.TaskID) == "" {
			return fmt.Errorf("task_id is required for agent role %q", e.Role)
		}
		if strings.TrimSpace(e.AttemptID) == "" {
			return fmt.Errorf("attempt_id is required for agent role %q", e.Role)
		}
	}
	if e.Type == AgentEventArtifactCreated && strings.TrimSpace(e.ArtifactID) == "" {
		return fmt.Errorf("artifact_id is required for agent event type %q", e.Type)
	}
	if e.Type == AgentEventInvocationFailed && strings.TrimSpace(e.ErrorMessage) == "" {
		return fmt.Errorf("error_message is required for agent event type %q", e.Type)
	}
	return nil
}
