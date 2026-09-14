# Complex manager and crash-recovery acceptance project

This project has three independent defect clusters plus one integration layer.
The checked-in implementation is intentionally wrong; Threadmill must repair only
its isolated task environments.

Acceptance contract:

1. `pricing.Total`
   - Rejects subtotals less than or equal to zero with `pricing.ErrInvalidSubtotal`.
   - Leaves positive subtotals below 10,000 cents unchanged.
   - Applies a 10% discount at 10,000 cents and above.
2. `inventory.Remaining`
   - Rejects requested quantities less than or equal to zero with `inventory.ErrInvalidQuantity`.
   - Rejects requests greater than available stock with `inventory.ErrInsufficientStock`.
   - Allows a request equal to available stock and returns zero remaining.
3. `shipping.Fee`
   - Rejects negative order amounts with `shipping.ErrInvalidAmount`.
   - Charges 500 cents below 9,000 cents.
   - Is free at 9,000 cents and above.
4. `checkout.Build`
   - Preserves and composes the three public APIs above.
   - Computes shipping from the discounted item total.
   - Returns `Total = Items + Shipping` and the correct remaining stock.
5. No public API changes, no weakened tests, and `go test ./...` passes.
