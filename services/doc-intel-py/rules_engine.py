# Author: Patrick Munis at newwave
"""Tenant-scoped rule engine — Python mirror of the Go engine
(services/case-api-go/cmd/server/rules.go). Semantics MUST stay identical:

- rules live in DATA: public.program_rules config->'rules'[]
- a rule: {name, event, enabled, conditions[], actions[]}
- conditions are conjunctive; empty conditions always match
- unknown ops/fields fail CLOSED: the rule never fires and is reported suspect
- actions: notify | log_activity | set_detail | flag_review | set_status
  (block_request is Go-handler-only; workers report it suspect here)

Workers read rules FRESH from Postgres per evaluation — a policy change in the
admin UI takes effect with no worker redeploy, same as the Go side.
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone, timedelta
from typing import Any

KNOWN_EVENTS = {
    "intake.advance", "doc.upload", "doc.analyzed",
    "claims.imported", "invoice.settled", "sweep.intake",
}
KNOWN_OPS = {
    "eq", "neq", "in", "contains", "gt", "gte", "lt", "lte",
    "is_null", "not_null", "days_older_than",
}
KNOWN_ACTIONS = {
    "set_status", "set_detail", "notify", "log_activity", "flag_review",
}

_DETAIL_KEY = re.compile(r"^[a-z][a-z0-9_]{0,40}$")


def _to_float(v: Any) -> float | None:
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, str):
        try:
            return float(v)
        except ValueError:
            return None
    return None


def eval_cond(cond: dict, facts: dict) -> tuple[bool, bool]:
    """Returns (ok, known). known=False => suspect rule, must not fire."""
    op = cond.get("op", "")
    field = cond.get("field", "")
    val = cond.get("value")
    fact = facts.get(field)

    if op == "is_null":
        return fact is None, True
    if op == "not_null":
        return fact is not None, True
    if op == "days_older_than":
        # fact is an RFC3339 timestamp; value is a day threshold
        if not isinstance(fact, str) or val is None:
            return False, True
        try:
            ts = datetime.fromisoformat(fact.replace("Z", "+00:00"))
        except ValueError:
            return False, True
        threshold = _to_float(val)
        if threshold is None:
            return False, False
        return (datetime.now(timezone.utc) - ts) > timedelta(days=threshold), True

    if op in ("gt", "gte", "lt", "lte"):
        f, v = _to_float(fact), _to_float(val)
        if f is None or v is None:
            return False, True
        return {"gt": f > v, "gte": f >= v, "lt": f < v, "lte": f <= v}[op], True
    if op == "eq":
        return fact == val, True
    if op == "neq":
        return fact != val, True
    if op == "in":
        return isinstance(val, list) and fact in val, True
    if op == "contains":
        if isinstance(fact, str) and isinstance(val, str):
            return val in fact, True
        if isinstance(fact, list):
            return val in fact, True
        return False, True
    return False, False  # unknown op -> suspect


def eval_rule(rule: dict, facts: dict) -> tuple[bool, bool]:
    for c in rule.get("conditions", []):
        ok, known = eval_cond(c, facts)
        if not known:
            return False, False
        if not ok:
            return False, True
    return True, True


def fire_rules(rules: list[dict], facts: dict):
    """Returns (actions, fired_names, suspect_names)."""
    actions, fired, suspect = [], [], []
    for ru in rules:
        if ru.get("enabled") is False:
            continue
        matched, understood = eval_rule(ru, facts)
        if not understood:
            suspect.append(ru.get("name", "?"))
            continue
        if matched:
            actions.extend(ru.get("actions", []))
            fired.append(ru.get("name", "?"))
    return actions, fired, suspect


def expand_template(tpl: str, facts: dict) -> str:
    for k, v in facts.items():
        tpl = tpl.replace("{{" + k + "}}", str(v))
    return tpl


def load_rules(conn, tenant: str, event: str) -> list[dict]:
    """Fresh read of the tenant's rules for one event from program_rules."""
    row = conn.execute(
        "SELECT config->'rules' FROM public.program_rules WHERE tenant=%s",
        (tenant,),
    ).fetchone()
    if not row or not row[0]:
        return []
    rules = row[0] if isinstance(row[0], list) else json.loads(row[0])
    return [r for r in rules if r.get("event") == event and r.get("enabled") is not False]
