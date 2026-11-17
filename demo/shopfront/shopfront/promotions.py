from dataclasses import dataclass
from datetime import date


@dataclass(frozen=True)
class Promotion:
    code: str
    percent_off: int
    expires_on: date

    def is_active(self) -> bool:
        return date.today() <= self.expires_on


def apply(promotion: Promotion, subtotal_cents: int) -> int:
    if not promotion.is_active():
        raise ValueError(f"promotion {promotion.code} has expired")
    return subtotal_cents * (100 - promotion.percent_off) // 100
