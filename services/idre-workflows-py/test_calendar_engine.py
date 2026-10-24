# Author: Patrick Munis at newwave
"""Unit tests for the business-day engine.

IDR deadlines (45 CFR 149 + 2026 Final Rule) are computed in business days —
a holiday/off-by-one bug here misses statutory clocks. Pin the federal holiday
table, observed shifts, per-state extras, and the arithmetic helpers.
"""

from __future__ import annotations

from datetime import date

import calendar_engine as cal


def test_federal_holidays_fixed_dates_2026():
    hols = cal.federal_holidays(2026)
    assert date(2026, 1, 1) in hols    # New Year's Day
    assert date(2026, 6, 19) in hols   # Juneteenth
    assert date(2026, 7, 4) in hols    # Independence Day
    assert date(2026, 11, 11) in hols  # Veterans Day
    assert date(2026, 12, 25) in hols  # Christmas
    assert len(hols) >= 11


def test_nth_weekday_holidays_2026():
    hols = cal.federal_holidays(2026)
    assert date(2026, 1, 19) in hols   # MLK (3rd Mon Jan)
    assert date(2026, 2, 16) in hols   # Washington's Birthday (3rd Mon Feb)
    assert date(2026, 9, 7) in hols    # Labor Day (1st Mon Sep)
    assert date(2026, 10, 12) in hols  # Columbus Day (2nd Mon Oct)
    assert date(2026, 11, 26) in hols  # Thanksgiving (4th Thu Nov)


def test_last_weekday_holiday_2026():
    assert date(2026, 5, 25) in cal.federal_holidays(2026)  # Memorial Day


def test_observed_shift_sunday_to_monday():
    # July 4, 2027 falls on Sunday -> observed Monday July 5.
    hols = cal.federal_holidays(2027)
    assert date(2027, 7, 4).weekday() == 6
    assert date(2027, 7, 5) in hols


def test_observed_shift_saturday_to_friday():
    # Christmas 2027 falls on Saturday -> observed Friday Dec 24.
    hols = cal.federal_holidays(2027)
    assert date(2027, 12, 25).weekday() == 5
    assert date(2027, 12, 24) in hols


def test_state_extras_extend_federal():
    tx = cal.holidays_for("tx", 2026)
    ca = cal.holidays_for("ca", 2026)
    fl = cal.holidays_for("fl", 2026)
    assert date(2026, 11, 27) in tx   # day after Thanksgiving
    assert date(2026, 3, 31) in ca    # Cesar Chavez Day
    assert date(2026, 11, 27) not in fl


def test_is_business_day():
    assert cal.is_business_day(date(2026, 10, 13), "fl")       # Tuesday
    assert not cal.is_business_day(date(2026, 10, 17), "fl")   # Saturday
    assert not cal.is_business_day(date(2026, 12, 25), "fl")   # Christmas


def test_add_business_days_skips_weekend_and_holiday():
    # Fri Nov 26, 2026 + 1 business day: Thanksgiving Nov 26, weekend 28-29
    # -> Monday Nov 30. (Starting day doesn't count.)
    start = date(2026, 11, 25)  # Wednesday
    assert cal.add_business_days(start, 1, "fl") == date(2026, 11, 27)  # Fri (not a federal holiday in fl)
    assert cal.add_business_days(start, 2, "fl") == date(2026, 11, 30)  # Mon


def test_add_business_days_state_extra_counts():
    # In tx, Nov 27 is a state holiday, so +1 from Nov 25 lands Nov 30.
    assert cal.add_business_days(date(2026, 11, 25), 1, "tx") == date(2026, 11, 30)


def test_business_days_between():
    # Mon Oct 12 -> Fri Oct 16, 2026: Tue,Wed,Thu,Fri = 4 business days.
    assert cal.business_days_between(date(2026, 10, 12), date(2026, 10, 16), "fl") == 4
    assert cal.business_days_between(date(2026, 10, 16), date(2026, 10, 12), "fl") == 0
