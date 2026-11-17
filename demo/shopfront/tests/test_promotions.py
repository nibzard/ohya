from datetime import datetime, timedelta, timezone

import pytest

from shopfront.promotions import Promotion, apply


def utc_today():
    return datetime.now(timezone.utc).date()


def test_active_code_takes_percent_off():
    promo = Promotion("SPRING10", 10, expires_on=utc_today() + timedelta(days=7))
    assert apply(promo, 5000) == 4500


def test_expired_code_is_rejected():
    promo = Promotion("SPRING10", 10, expires_on=utc_today() - timedelta(days=1))
    with pytest.raises(ValueError):
        apply(promo, 5000)
