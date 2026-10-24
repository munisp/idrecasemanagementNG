# Author: Patrick Munis at newwave
"""Unit tests for the pure analytics logic — no Spark/Flink/Ray cluster
needed: lakehouse_common is dependency-light by design, and flink_stream
guards its pyflink imports so SLAWatch/to_row stay importable.

Run: python -m pytest test_lakehouse_common.py -q
"""

from __future__ import annotations

import hashlib
from datetime import date

import pytest

from lakehouse_common import (
    MIN_TRAIN_SAMPLES, event_fingerprint, extract_case_features,
    logistic_fit, logistic_predict_proba, month_bounds,
    validate_readonly_sql, wrap_with_row_cap,
)
from flink_stream import DOMAIN_BY_PREFIX, SLAWatch, to_row

# ---------------------------------------------------------------------------
# SQL gate
# ---------------------------------------------------------------------------


@pytest.mark.parametrize("sql", [
    "select * from cms_monthly_report",
    "SELECT tenant, count(*) FROM qpa_benchmarks GROUP BY tenant",
    "with x as (select 1) select * from x",
    "select ';' as semi",                      # semicolon inside a literal
    "select * from t -- harmless comment\n",
    "select name from t where name = 'drop table x'",
    "sel/**/ect 1",   # strips to a genuine read-only select: engine-safe
])
def test_readonly_gate_accepts(sql):
    assert validate_readonly_sql(sql) is None


@pytest.mark.parametrize("sql", [
    "",
    "   ",
    "select 1; select 2",                      # stacked statements
    "insert into t values (1)",
    "select * from t; drop table t",
    "update t set x = 1",
    "select 1 /* hidden */ ; delete from t",
    "copy t from 's3://evil'",
    "show tables",                             # not SELECT/WITH
    "select 1; dr/**/op table t",              # comment-spliced blocked keyword
])
def test_readonly_gate_rejects(sql):
    assert validate_readonly_sql(sql) is not None


def test_row_cap_wraps_and_strips_semicolon():
    wrapped = wrap_with_row_cap("select 1;", 100)
    assert wrapped == "SELECT * FROM (select 1) AS _capped LIMIT 101"


# ---------------------------------------------------------------------------
# Fingerprint
# ---------------------------------------------------------------------------


def test_fingerprint_matches_sha256_contract():
    tenant, at, payload = "tx", "2026-01-01T00:00:00Z", '{"type":"case.initiated"}'
    expect = hashlib.sha256(f"{tenant}|{at}|{payload}".encode()).hexdigest()
    assert event_fingerprint(tenant, at, payload) == expect
    # deterministic + sensitive to every component
    assert event_fingerprint(tenant, at, payload) == expect
    assert event_fingerprint("ca", at, payload) != expect
    assert event_fingerprint(tenant, at + "x", payload) != expect


# ---------------------------------------------------------------------------
# Month bounds
# ---------------------------------------------------------------------------


def test_month_bounds_rollover():
    assert month_bounds("2026-01") == (date(2026, 1, 1), date(2026, 2, 1))
    assert month_bounds("2026-12") == (date(2026, 12, 1), date(2027, 1, 1))
    with pytest.raises(ValueError):
        month_bounds("2026-13")


# ---------------------------------------------------------------------------
# Case features / labels
# ---------------------------------------------------------------------------


def _row(case_id, etype, status="", payload="{}"):
    return {"case_id": case_id, "etype": etype, "status": status,
            "payload": payload}


def test_extract_case_features_labels():
    rows = [
        _row("a", "case.initiated", payload='{"qpa_cents": 150000}'),
        _row("a", "offer.submitted"),
        _row("a", "case.status", status="SETTLED_IN_NEGOTIATION"),
        _row("b", "case.initiated", payload='{"qpa_cents": 90000}'),
        _row("b", "case.status", status="DETERMINED"),
        _row("c", "case.initiated"),          # open: excluded from training
        _row("d", "case.status", status="SETTLED_IN_NEGOTIATION"),
        _row("d", "case.status", status="DETERMINED"),   # contradictory: skip
        _row("", "case.initiated"),           # no case id: skip
    ]
    X, y, n_open = extract_case_features(rows)
    assert y == [1, 0]
    assert n_open == 1
    assert X[0] == [1500.0, 1.0, 3.0]   # qpa_usd, offers, events
    assert X[1] == [900.0, 0.0, 2.0]


# ---------------------------------------------------------------------------
# Logistic regression
# ---------------------------------------------------------------------------


def test_logistic_deterministic_and_converges():
    import numpy as np

    rng = np.arange(400, dtype=float)
    X = np.stack([rng, rng % 7, rng % 13], axis=1)
    y = (rng > 200).astype(float)
    w1, b1, mu1, sd1 = logistic_fit(X, y)
    w2, b2, mu2, sd2 = logistic_fit(X, y)
    assert np.array_equal(w1, w2) and b1 == b2   # bit-for-bit deterministic
    p = logistic_predict_proba(X, w1, b1, mu1, sd1)
    assert float(((p > 0.5) == y).mean()) > 0.99
    assert p.min() >= 0 and p.max() <= 1


def test_logistic_constant_column_survives():
    import numpy as np

    X = np.array([[1.0, 0.0], [2.0, 0.0], [3.0, 0.0], [4.0, 0.0]])
    y = np.array([0.0, 0.0, 1.0, 1.0])
    w, b, mu, sd = logistic_fit(X, y)
    assert sd[1] == 1.0                          # zero-variance guard
    p = logistic_predict_proba(X, w, b, mu, sd)
    assert float(((p > 0.5) == y).mean()) == 1.0


# ---------------------------------------------------------------------------
# Flink pure pieces
# ---------------------------------------------------------------------------


def test_to_row_domain_routing():
    row = to_row('{"type":"case.initiated","tenant":"tx","at":"2026-01-01T00:00:00Z"}')
    assert row == ("2026-01-01T00:00:00Z", "tx", "cases",
                   '{"type":"case.initiated","tenant":"tx","at":"2026-01-01T00:00:00Z"}')
    # unmapped prefix -> fallback domain
    assert to_row('{"type":"sla.breach","tenant":"tx","at":"t"}')[2] == "events"
    assert to_row('{"type":"audit.append","tenant":"tx","at":"t"}')[2] == "audit"
    # malformed / tenant-less rows are dropped
    assert to_row("not json") is None
    assert to_row('{"type":"case.initiated"}') is None
    # every documented prefix maps to its own bronze domain
    assert set(DOMAIN_BY_PREFIX.values()) == {
        "cases", "offers", "fees", "rules", "documents", "audit"}


def test_sla_watch_window_math():
    future = {"offer_window_ends_at": "2999-01-10"}
    now = SLAWatch.deadline_ms("2999-01-01")
    # 9 days out: not due, timer armed 2 days ahead of the deadline
    assert SLAWatch.due(future, now) == []
    assert SLAWatch.next_timer_ms(future, now) == \
        SLAWatch.deadline_ms("2999-01-10") - SLAWatch.WARN_MS
    # 1 day out: due, no timer
    later = SLAWatch.deadline_ms("2999-01-09")
    due = SLAWatch.due(future, later)
    assert due == [("offer_window_ends_at",
                    SLAWatch.deadline_ms("2999-01-10"), "2999-01-10")]
    assert SLAWatch.next_timer_ms(future, later) is None
    # garbage dates never fire
    assert SLAWatch.due({"offer_window_ends_at": "n/a"}, later) == []
