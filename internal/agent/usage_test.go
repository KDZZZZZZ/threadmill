package agent

import "testing"

func TestUsageContextTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		usage    Usage
		expected int
	}{
		{
			name:     "prefers total tokens",
			usage:    Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 9},
			expected: 9,
		},
		{
			name: "sums components when total is missing",
			usage: Usage{
				InputTokens:      3,
				CachedTokens:     4,
				CacheWriteTokens: 5,
				OutputTokens:     6,
				ReasoningTokens:  7,
			},
			expected: 25,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.usage.ContextTokens(); got != tt.expected {
				t.Fatalf("ContextTokens() = %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestShouldCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		usage         *Usage
		contextWindow int
		expected      bool
	}{
		{
			name:          "nil usage",
			contextWindow: 100,
			expected:      false,
		},
		{
			name:          "disabled window",
			usage:         &Usage{TotalTokens: 50},
			contextWindow: 0,
			expected:      false,
		},
		{
			name:          "negative window",
			usage:         &Usage{TotalTokens: 50},
			contextWindow: -1,
			expected:      false,
		},
		{
			name:          "at window",
			usage:         &Usage{TotalTokens: 100},
			contextWindow: 100,
			expected:      false,
		},
		{
			name:          "over window",
			usage:         &Usage{TotalTokens: 101},
			contextWindow: 100,
			expected:      true,
		},
		{
			name:          "component sum over window",
			usage:         &Usage{InputTokens: 80, OutputTokens: 30},
			contextWindow: 100,
			expected:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ShouldCompact(tt.usage, tt.contextWindow); got != tt.expected {
				t.Fatalf("ShouldCompact() = %v, want %v", got, tt.expected)
			}
		})
	}
}
