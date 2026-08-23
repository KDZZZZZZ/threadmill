package agent

import "testing"

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
			name:          "below pressure threshold",
			usage:         &Usage{TotalTokens: 74},
			contextWindow: 100,
			expected:      false,
		},
		{
			name:          "at pressure threshold",
			usage:         &Usage{TotalTokens: 75},
			contextWindow: 100,
			expected:      true,
		},
		{
			name:          "at window",
			usage:         &Usage{TotalTokens: 100},
			contextWindow: 100,
			expected:      true,
		},
		{
			name:          "over window",
			usage:         &Usage{TotalTokens: 101},
			contextWindow: 100,
			expected:      true,
		},
		{
			name:          "missing total does not compact",
			usage:         &Usage{InputTokens: 80, CachedTokens: 40, OutputTokens: 30, ReasoningTokens: 10},
			contextWindow: 100,
			expected:      false,
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
