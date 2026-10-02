import pytest
from pricing import shipping


@pytest.mark.parametrize("amount,expected", [(0, 500), (8_999, 500), (9_000, 0), (12_000, 0)])
def test_shipping(amount, expected):
    assert shipping(amount) == expected


def test_shipping_rejects_negative():
    with pytest.raises(ValueError):
        shipping(-1)
