package inventory

import (
	"errors"
	"testing"
)

func TestRemainingBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		available int
		requested int
		want      int
		wantErr   error
	}{
		{name: "negative", available: 3, requested: -1, wantErr: ErrInvalidQuantity},
		{name: "zero", available: 3, requested: 0, wantErr: ErrInvalidQuantity},
		{name: "partial", available: 3, requested: 2, want: 1},
		{name: "all stock", available: 3, requested: 3, want: 0},
		{name: "too much", available: 3, requested: 4, wantErr: ErrInsufficientStock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Remaining(tt.available, tt.requested)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Remaining(%d, %d) = %d, want %d", tt.available, tt.requested, got, tt.want)
			}
		})
	}
}
