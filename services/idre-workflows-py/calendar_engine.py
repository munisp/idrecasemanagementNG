# Author: Patrick Munis at newwave
"""Business-day engine: US federal holidays (observed shifts, nth-weekday) +
per-state extra dates, operating on date objects. Mirrors contracts/idr.ts."""

from __future__ import annotations

from datetime import date, timedelta

EASTER_CACHE: dict[int, date] = {}


def _easter(year: int) -> date:
    if year in EASTER_CACHE:
        return EASTER_CACHE[year]
    a, b, c = year % 19, year // 100, year % 100
    d, e = b // 4, b % 4
    f = (b + 8) // 25
    g = (b - f + 1) // 3
    h = (19 * a + b - d - g + 15) % 30
    i, k = c // 4, c % 4
    l = (32 + 2 * e + 2 * i - h - k) % 7
    m = (a + 11 * h + 22 * l) // 451
    month, day = (h + l - 7 * m + 114) // 31, (h + l - 7 * m + 114) % 31 + 1
    EASTER_CACHE[year] = date(year, month, day)
    return EASTER_CACHE[year]


def _nth_weekday(year: int, month: int, weekday: int, n: int) -> date:
    d = date(year, month, 1)
    while d.weekday() != weekday:
        d += timedelta(days=1)
    return d + timedelta(weeks=n - 1)


def _last_weekday(year: int, month: int, weekday: int) -> date:
    d = date(year, month + 1, 1) - timedelta(days=1) if month < 12 else date(year, 12, 31)
    while d.weekday() != weekday:
        d -= timedelta(days=1)
    return d


def federal_holidays(year: int) -> set[date]:
    """US federal holidays with observed shifts (Sat->Fri, Sun->Mon)."""
    raw = [
        date(year, 1, 1),                       # New Year's Day
        _nth_weekday(year, 1, 0, 3),            # MLK
        _nth_weekday(year, 2, 0, 3),            # Washington's Birthday
        _last_weekday(year, 5, 0),              # Memorial Day
        date(year, 6, 19),                      # Juneteenth
        date(year, 7, 4),                       # Independence Day
        _nth_weekday(year, 9, 0, 1),            # Labor Day
        _nth_weekday(year, 10, 0, 2),           # Columbus Day
        date(year, 11, 11),                     # Veterans Day
        _nth_weekday(year, 11, 3, 4),           # Thanksgiving
        date(year, 12, 25),                     # Christmas
    ]
    out: set[date] = set()
    for d in raw:
        if d.weekday() == 5:      # Saturday -> observed Friday
            out.add(d - timedelta(days=1))
        elif d.weekday() == 6:    # Sunday -> observed Monday
            out.add(d + timedelta(days=1))
        out.add(d)
    return out


# Per-state extra non-business dates (research-driven; extend via state_config).
STATE_EXTRA: dict[str, list[tuple[int, int]]] = {
    "tx": [(11, 27)],  # day after Thanksgiving (approx; state holiday)
    "ca": [(3, 31)],   # Cesar Chavez Day
}


def holidays_for(tenant: str, year: int) -> set[date]:
    days = federal_holidays(year)
    for m, d in STATE_EXTRA.get(tenant, []):
        days.add(date(year, m, d))
    return days


def is_business_day(d: date, tenant: str) -> bool:
    return d.weekday() < 5 and d not in holidays_for(tenant, d.year)


def add_business_days(start: date, n: int, tenant: str) -> date:
    d, added = start, 0
    while added < n:
        d += timedelta(days=1)
        if is_business_day(d, tenant):
            added += 1
    return d


def business_days_between(a: date, b: date, tenant: str) -> int:
    d, n = a, 0
    while d < b:
        d += timedelta(days=1)
        if is_business_day(d, tenant):
            n += 1
    return n
