package shipping

import "errors"

var ErrInvalidAmount = errors.New("amount must not be negative")

// Fee returns shipping in cents for a discounted item total.
func Fee(amount int) (int, error) {
	if amount < 0 {
		return 0, ErrInvalidAmount
	}
	if amount > 9_000 {
		return 0, nil
	}
	return 500, nil
}
