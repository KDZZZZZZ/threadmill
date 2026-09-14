package checkout

import (
	"errors"
	"testing"

	"example.com/threadmill-crash-acceptance/inventory"
	"example.com/threadmill-crash-acceptance/pricing"
)

func TestBuildDiscountedFreeShippingAtBoundary(t *testing.T) {
	got, err := Build(10_000, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := Quote{Items: 9_000, Shipping: 0, Total: 9_000, Remaining: 0}
	if got != want {
		t.Fatalf("Build() = %#v, want %#v", got, want)
	}
}

func TestBuildAddsPaidShippingToDiscountedItems(t *testing.T) {
	got, err := Build(5_000, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := Quote{Items: 5_000, Shipping: 500, Total: 5_500, Remaining: 3}
	if got != want {
		t.Fatalf("Build() = %#v, want %#v", got, want)
	}
}

func TestBuildForwardsValidationErrors(t *testing.T) {
	if _, err := Build(0, 5, 1); !errors.Is(err, pricing.ErrInvalidSubtotal) {
		t.Fatalf("subtotal error = %v, want %v", err, pricing.ErrInvalidSubtotal)
	}
	if _, err := Build(5_000, 5, 0); !errors.Is(err, inventory.ErrInvalidQuantity) {
		t.Fatalf("quantity error = %v, want %v", err, inventory.ErrInvalidQuantity)
	}
}
