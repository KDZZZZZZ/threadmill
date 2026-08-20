package pricing

import "errors"

var ErrInvalidSubtotal = errors.New("subtotal must be positive")

// Total returns the amount due in cents after applying the volume discount.
func Total(subtotal int) (int, error) {
	if subtotal < 0 {
		return 0, ErrInvalidSubtotal
	}
	if subtotal > 10_000 {
		return subtotal * 90 / 100, nil
	}
	return subtotal, nil
}
