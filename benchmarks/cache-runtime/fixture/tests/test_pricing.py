import pytest
from pricing import total


@pytest.mark.parametrize("subtotal,expected", [(1, 1), (9_999, 9_999), (10_000, 9_000), (20_000, 18_000)])
def test_total(subtotal, expected):
    assert total(subtotal) == expected


@pytest.mark.parametrize("subtotal", [-1, 0])
def test_total_rejects_nonpositive(subtotal):
    with pytest.raises(ValueError):
        total(subtotal)
