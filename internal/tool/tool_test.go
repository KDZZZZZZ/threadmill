package tool

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDefinitionValidate(t *testing.T) {
	tests := []struct {
		name       string
		definition Definition
		expected   error
	}{
		{
			name: "valid definition",
			definition: Definition{
				Name:        "echo",
				Description: "Echo text",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		{
			name: "missing name",
			definition: Definition{
				Description: "Echo text",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			expected: ErrInvalidDefinition,
		},
		{
			name: "schema is not an object",
			definition: Definition{
				Name:        "echo",
				Description: "Echo text",
				InputSchema: json.RawMessage(`[]`),
			},
			expected: ErrInvalidDefinition,
		},
		{
			name: "name contains a dot",
			definition: Definition{
				Name:        "coordination.replacePending",
				Description: "replace pending",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
			expected: ErrInvalidDefinition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.definition.Validate()
			if !errors.Is(err, tt.expected) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.expected)
			}
		})
	}
}

func TestCallValidate(t *testing.T) {
	tests := []struct {
		name     string
		call     Call
		expected error
	}{
		{
			name: "valid call",
			call: Call{
				ID:        "call-1",
				Name:      "echo",
				Arguments: json.RawMessage(`{"text":"hello"}`),
			},
		},
		{
			name: "empty arguments mean empty object",
			call: Call{ID: "call-1", Name: "echo"},
		},
		{
			name:     "missing call id",
			call:     Call{Name: "echo", Arguments: json.RawMessage(`{}`)},
			expected: ErrInvalidCall,
		},
		{
			name:     "arguments are not an object",
			call:     Call{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`"hello"`)},
			expected: ErrInvalidCall,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call.Validate()
			if !errors.Is(err, tt.expected) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.expected)
			}
		})
	}
}
