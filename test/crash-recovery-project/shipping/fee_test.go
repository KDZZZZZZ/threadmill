package shipping

import (
	"errors"
	"testing"
)

func TestFeeBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		amount  int
		want    int
		wantErr error
	}{
		{name: "negative", amount: -1, wantErr: ErrInvalidAmount},
		{name: "zero", amount: 0, want: 500},
		{name: "below threshold", amount: 8_999, want: 500},
		{name: "at threshold", amount: 9_000, want: 0},
		{name: "above threshold", amount: 12_000, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Fee(tt.amount)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Fee(%d) = %d, want %d", tt.amount, got, tt.want)
			}
		})
	}
}
