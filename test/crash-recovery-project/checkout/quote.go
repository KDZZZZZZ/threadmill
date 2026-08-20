package checkout

import (
	"example.com/threadmill-crash-acceptance/inventory"
	"example.com/threadmill-crash-acceptance/pricing"
	"example.com/threadmill-crash-acceptance/shipping"
)

type Quote struct {
	Items     int
	Shipping  int
	Total     int
	Remaining int
}

// Build validates stock and returns a complete checkout quote.
func Build(subtotal, available, requested int) (Quote, error) {
	remaining, err := inventory.Remaining(available, requested)
	if err != nil {
		return Quote{}, err
	}
	items, err := pricing.Total(subtotal)
	if err != nil {
		return Quote{}, err
	}
	delivery, err := shipping.Fee(subtotal)
	if err != nil {
		return Quote{}, err
	}
	return Quote{Items: items, Shipping: delivery, Total: subtotal + delivery, Remaining: remaining}, nil
}
