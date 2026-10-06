"""Unit tests for manifest clock resolution — pure, no Temporal sandbox."""
from datetime import date

import case_clocks as cc


def test_absent_manifest_is_nsa_defaults():
    r = cc.resolve(None)
    assert r == cc.NSA_DEFAULTS
    assert cc.resolve({}) == cc.NSA_DEFAULTS
    assert cc.resolve([]) == cc.NSA_DEFAULTS


def test_manifest_overrides_only_named_clocks():
    # TX auto appraisal: 75-day calendar umpire decision window
    r = cc.resolve([{"name": "determination_window", "days": 75, "day_type": "calendar"},
                    {"name": "payment_window", "days": 45, "day_type": "calendar"},
                    {"name": "outreach_window", "days": 13, "day_type": "business"}])
    assert r["determination_window"] == (75, "calendar")
    assert r["payment_window"] == (45, "calendar")
    assert r["response_window"] == (3, "business")   # untouched default
    assert "outreach_window" not in r                # non-workflow clock ignored


def test_dict_form_accepted():
    r = cc.resolve({"offer_window": {"days": 20, "day_type": "calendar"}})
    assert r["offer_window"] == (20, "calendar")


def test_malformed_entries_fall_back_safely():
    r = cc.resolve([{"name": "offer_window", "days": -5, "day_type": "business"},
                    {"name": "offer_window", "days": 10, "day_type": "fortnight"},
                    {"name": "response_window", "days": True, "day_type": "business"},
                    "garbage", None])
    assert r == cc.NSA_DEFAULTS


def test_feature_gate_parity_with_portal():
    assert cc.feature(None, "sealed_offers") is True            # legacy tenant
    assert cc.feature({}, "sealed_offers") is True              # NSA default
    assert cc.feature({}, "medical_review") is False            # non-NSA default off
    assert cc.feature({"sealed_offers": False}, "sealed_offers") is False
    assert cc.feature({"voice_console": True}, "voice_console") is True


def test_calendar_deadline():
    assert cc.deadline(date(2026, 1, 1), 30, "calendar") == date(2026, 1, 31)


def test_business_deadline_skips_weekends_and_holidays():
    # Fri Jan 2 2026 + 3 business days = Thu Jan 8 (skip Sat/Sun, Mon Jan 5 counts)
    assert cc.deadline(date(2026, 1, 2), 3, "business") == date(2026, 1, 7)
    hol = {date(2026, 1, 5)}
    assert cc.deadline(date(2026, 1, 2), 3, "business", hol) == date(2026, 1, 8)


def test_business_deadline_empty_holidays_ok():
    assert cc.deadline(date(2026, 7, 2), 1, "business", set()) == date(2026, 7, 3)
