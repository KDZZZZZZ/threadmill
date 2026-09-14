# Threadmill real-model acceptance project

This deliberately small Go project exercises the complete manager → planner → executor → verifier path against a real model.

Acceptance behavior for `pricing.Total`:

- Subtotals less than or equal to zero return `pricing.ErrInvalidSubtotal`.
- Subtotals below 10,000 cents are unchanged.
- Subtotals of 10,000 cents or more receive a 10% discount.
- The existing public API remains unchanged.
- `go test ./...` passes.

The checked-in implementation intentionally violates two boundary cases. Threadmill task environments are isolated, so an acceptance run fixes and verifies its virtual workspace without changing this fixture on the host.

