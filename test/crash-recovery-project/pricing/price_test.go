package pricing

import (
	"errors"
	"testing"
)

func TestTotalBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		in      int
		want    int
		wantErr error
	}{
		{name: "negative", in: -1, wantErr: ErrInvalidSubtotal},
		{name: "zero", in: 0, wantErr: ErrInvalidSubtotal},
		{name: "below threshold", in: 9_999, want: 9_999},
		{name: "at threshold", in: 10_000, want: 9_000},
		{name: "above threshold", in: 20_000, want: 18_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Total(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Total(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
