def total(subtotal):
    if subtotal <= 0:
        raise ValueError("subtotal must be positive")
    return subtotal * 90 // 100 if subtotal >= 10_000 else subtotal


def shipping(amount):
    if amount < 0:
        raise ValueError("amount must not be negative")
    return 0 if amount >= 9_000 else 500
