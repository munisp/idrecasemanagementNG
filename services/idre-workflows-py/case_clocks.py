"""Manifest-driven statutory clock resolution for case workflows.

Pure functions only — no I/O, no Temporal imports — so the resolution logic
is unit-testable without a workflow sandbox and can be mirrored by other
workers. The workflow loads the raw manifest block once via an activity
(recorded in history, so replay stays deterministic) and every phase timeout
derives from resolve() below.

Reserved workflow clock names in manifest.clocks:
    response_window       non-initiating party response     (NSA: 3 business)
    offer_window          double-blind offer submission     (NSA: 10 business)
    selection_window      neutral selection                 (NSA: 10 calendar)
    determination_window  neutral's written determination   (NSA: 30 business)
    payment_window        non-prevailing party pays         (NSA: 30 calendar)

Absent manifest or absent clock name → the NSA default below (legacy tenants
keep identical behavior). Unknown/malformed entries are ignored in favor of
the default — validate-on-write in case-api already rejects malformed clocks,
so this is only a defense-in-depth layer.
"""

from __future__ import annotations

from datetime import date, timedelta

NSA_DEFAULTS: dict[str, tuple[int, str]] = {
    "response_window": (3, "business"),
    "offer_window": (10, "business"),
    "selection_window": (10, "calendar"),
    "determination_window": (30, "business"),
    "payment_window": (30, "calendar"),
}

_DAY_TYPES = ("business", "calendar")


def resolve(manifest_clocks: dict | None) -> dict[str, tuple[int, str]]:
    """Manifest clock list/dict → {name: (days, day_type)} over all reserved names.

    Accepts the manifest's clocks array form ([{name, days, day_type, ...}])
    or a pre-keyed dict; anything unrecognized falls back to NSA_DEFAULTS.
    """
    out = dict(NSA_DEFAULTS)
    if not manifest_clocks:
        return out
    entries = manifest_clocks
    if isinstance(entries, dict):  # tolerate {name: {days, day_type}} form
        entries = [{"name": k, **(v or {})} for k, v in entries.items()]
    if not isinstance(entries, list):
        return out
    for c in entries:
        if not isinstance(c, dict):
            continue
        name, days, dt = c.get("name"), c.get("days"), c.get("day_type")
        if name in out and isinstance(days, int) and not isinstance(days, bool) \
                and days > 0 and dt in _DAY_TYPES:
            out[name] = (days, dt)
    return out


def feature(features: dict | None, name: str) -> bool:
    """Portal parity: no manifest → legacy (all on); manifest present →
    declared value, else NSA default (sealed_offers/negotiation_window on)."""
    if features is None:
        return True
    if name in features:
        return bool(features[name])
    return name in ("sealed_offers", "negotiation_window")


def deadline(start: date, days: int, day_type: str,
             holidays: set[date] | None = None) -> date:
    """Calendar deadline date for a clock expiring `days` after `start`.

    business  → skip weekends and tenant holidays (from public.holidays via
                the loader activity; empty set = weekends only)
    calendar  → plain date addition
    """
    if day_type == "calendar":
        return start + timedelta(days=days)
    hol = holidays or set()
    d, added = start, 0
    while added < days:
        d += timedelta(days=1)
        if d.weekday() < 5 and d not in hol:
            added += 1
    return d
