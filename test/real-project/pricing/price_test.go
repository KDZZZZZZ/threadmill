package pricing

import (
	"errors"
	"testing"
)

func TestTotal(t *testing.T) {
	tests := []struct {
		name     string
		subtotal int
		want     int
		wantErr  error
	}{
		{name: "negative is invalid", subtotal: -1, wantErr: ErrInvalidSubtotal},
		{name: "zero is invalid", subtotal: 0, wantErr: ErrInvalidSubtotal},
		{name: "below threshold is unchanged", subtotal: 9_999, want: 9_999},
		{name: "threshold is discounted", subtotal: 10_000, want: 9_000},
		{name: "above threshold is discounted", subtotal: 25_000, want: 22_500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Total(tt.subtotal)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Total(%d) error = %v, want %v", tt.subtotal, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Total(%d) = %d, want %d", tt.subtotal, got, tt.want)
			}
		})
	}
}

