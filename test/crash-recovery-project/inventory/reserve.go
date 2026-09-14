package inventory

import "errors"

var (
	ErrInvalidQuantity   = errors.New("quantity must be positive")
	ErrInsufficientStock = errors.New("insufficient stock")
)

// Remaining validates a reservation and returns the remaining stock.
func Remaining(available, requested int) (int, error) {
	if requested < 0 {
		return 0, ErrInvalidQuantity
	}
	if requested >= available {
		return 0, ErrInsufficientStock
	}
	return available - requested, nil
}
