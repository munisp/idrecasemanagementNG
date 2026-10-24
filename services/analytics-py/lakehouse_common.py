"""lakehouse_common: pure, dependency-light logic shared by the analytics
jobs — kept free of Spark/Flink/Ray imports so it is unit-testable in CI
without a cluster. Everything here is deterministic.

Gates follow the platform-wide learning discipline: no learned or
statistical surface reports below its sample floor, and every surface says
so instead of quoting a number it hasn't earned.
"""

from __future__ import annotations

import hashlib
import re
from datetime import date

# Same floor as the case-api model gate: below this many samples a learned
# surface reports insufficient_data rather than a confident-sounding number.
MIN_TRAIN_SAMPLES = 300

# ---------------------------------------------------------------------------
# Read-only SQL gate (DataFusion service)
# ---------------------------------------------------------------------------

# Word-boundary blocklist: anything that mutates state, touches the local
# filesystem, or escapes the registered gold-zone relations.
_BLOCKED = re.compile(
    r"\b(insert|update|delete|drop|create|alter|truncate|merge|grant|revoke|"
    r"copy|load|install|attach|detach|pragma|call|use|set|reset|vacuum|"
    r"optimize|explain)\b",
    re.IGNORECASE,
)


def _strip_literals_and_comments(sql: str) -> str:
    """Replace string literals and comments with spaces, preserving length
    semantics enough for statement counting. Single quotes with '' escapes
    and /* */ + -- comments are handled; a semicolon inside a literal must
    NOT count as a statement separator.
    """
    out: list[str] = []
    i, n = 0, len(sql)
    while i < n:
        c = sql[i]
        if c == "'":
            i += 1
            while i < n:
                if sql[i] == "'":
                    if i + 1 < n and sql[i + 1] == "'":
                        i += 2
                        continue
                    i += 1
                    break
                i += 1
            out.append(" ")
        elif c == "-" and i + 1 < n and sql[i + 1] == "-":
            while i < n and sql[i] != "\n":
                i += 1
        elif c == "/" and i + 1 < n and sql[i + 1] == "*":
            end = sql.find("*/", i + 2)
            i = n if end == -1 else end + 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


def validate_readonly_sql(sql: str) -> str | None:
    """Return None when the statement is a single read-only SELECT/CTE,
    else a human-readable rejection reason. The old gate (regex + raw
    semicolon check) rejected nothing with a quoted ';' and let comments
    smuggle keywords past the scan; this one scans the stripped text.
    """
    if not sql or not sql.strip():
        return "empty statement"
    stripped = _strip_literals_and_comments(sql)
    if ";" in stripped.strip().rstrip(";"):
        return "one statement per query"
    head = stripped.lstrip()[:8].lower()
    if not (head.startswith("select") or head.startswith("with")):
        return "read-only SELECT only"
    m = _BLOCKED.search(stripped)
    if m:
        return f"blocked keyword: {m.group(0).upper()}"
    return None


def wrap_with_row_cap(sql: str, max_rows: int) -> str:
    """Hard row cap enforced by the engine, not the client."""
    inner = sql.strip().rstrip(";")
    return f"SELECT * FROM ({inner}) AS _capped LIMIT {int(max_rows) + 1}"


# ---------------------------------------------------------------------------
# Event identity (idempotent silver merges)
# ---------------------------------------------------------------------------


def event_fingerprint(tenant: str, event_time: str, payload: str) -> str:
    """Stable identity for one bronze event. The outbox has no globally
    unique event id, so identity is content-derived; the silver MERGE keys
    on this, making replays and overlapping windows harmless."""
    h = hashlib.sha256()
    h.update(tenant.encode())
    h.update(b"|")
    h.update(event_time.encode())
    h.update(b"|")
    h.update(payload.encode())
    return h.hexdigest()


# ---------------------------------------------------------------------------
# Month scoping (CMS report correctness)
# ---------------------------------------------------------------------------


def month_bounds(month: str) -> tuple[date, date]:
    """[first, first_of_next) bounds for a YYYY-MM month. All report metrics
    filter the EVENT date into this window — closures count in the month
    they closed, not the month their case happened to open."""
    y, m = int(month[:4]), int(month[5:7])
    if not 1 <= m <= 12:
        raise ValueError(f"bad month {month!r}")
    first = date(y, m, 1)
    nxt = date(y + 1, 1, 1) if m == 12 else date(y, m + 1, 1)
    return first, nxt


# ---------------------------------------------------------------------------
# Case features (Ray training job) — pure label/feature extraction
# ---------------------------------------------------------------------------


def extract_case_features(rows) -> tuple[list[list[float]], list[int], int]:
    """(X, y, n_open) from silver/cases rows for one tenant.

    Label: 1 if the case ever reached SETTLED_IN_NEGOTIATION, 0 if it
    reached DETERMINED. Cases with neither terminal state are open and do
    not train (counted in n_open for observability); cases with both are
    contradictory history and skipped rather than guessed at. Features are
    numeric, interpretable, and known at intake time: qpa_usd, offer event
    count, total event count."""
    import json as _json

    by_case: dict[str, dict] = {}
    for r in rows:
        case_id = r.get("case_id")
        if not case_id:
            continue
        c = by_case.setdefault(case_id, {"qpa_usd": 0.0, "offers": 0,
                                         "events": 0, "settled": False,
                                         "determined": False})
        c["events"] += 1
        etype = r.get("etype") or ""
        status = r.get("status") or ""
        if etype == "case.initiated":
            try:
                payload = _json.loads(r.get("payload") or "{}")
                c["qpa_usd"] = float(payload.get("qpa_cents") or 0) / 100
            except (ValueError, TypeError):
                pass
        if etype.startswith("offer."):
            c["offers"] += 1
        if status == "SETTLED_IN_NEGOTIATION":
            c["settled"] = True
        if status == "DETERMINED":
            c["determined"] = True

    X: list[list[float]] = []
    y: list[int] = []
    n_open = 0
    for c in by_case.values():
        if c["settled"] and c["determined"]:
            continue
        if not c["settled"] and not c["determined"]:
            n_open += 1
            continue
        X.append([c["qpa_usd"], float(c["offers"]), float(c["events"])])
        y.append(1 if c["settled"] else 0)
    return X, y, n_open


# ---------------------------------------------------------------------------
# Logistic regression (Ray training job) — numpy only, deterministic
# ---------------------------------------------------------------------------


def logistic_fit(X, y, lr: float = 0.1, epochs: int = 2000, l2: float = 1e-3):
    """Full-batch gradient descent for a binary logistic model. Features are
    standardized inside (mean/std returned for inference); deterministic —
    no random init, no sampling — so a retrain on the same data reproduces
    the same parameters bit for bit."""
    import numpy as np

    X = np.asarray(X, dtype=float)
    y = np.asarray(y, dtype=float)
    mu = X.mean(axis=0)
    sd = X.std(axis=0)
    sd[sd == 0] = 1.0
    Xs = (X - mu) / sd
    w = np.zeros(Xs.shape[1])
    b = 0.0
    n = len(y)
    for _ in range(epochs):
        z = Xs @ w + b
        p = 1.0 / (1.0 + np.exp(-z))
        err = p - y
        w -= lr * (Xs.T @ err / n + l2 * w)
        b -= lr * float(err.mean())
    return w, b, mu, sd


def logistic_predict_proba(X, w, b, mu, sd):
    import numpy as np

    Xs = (np.asarray(X, dtype=float) - mu) / sd
    z = Xs @ w + b
    return 1.0 / (1.0 + np.exp(-z))
