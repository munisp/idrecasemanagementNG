"""Temporal activities: side effects go here (DB updates, ledger postings,
vault reveal calls, notifications). Workflows stay deterministic."""

from __future__ import annotations

import os

import httpx
import psycopg
from temporalio import activity

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")
VAULT_URL = os.environ.get("VAULT_URL", "http://localhost:8081")
NOTIFY_URL = os.environ.get("NOTIFY_URL", "http://localhost:8090")


def _conn():
    return psycopg.connect(DSN, autocommit=True)


@activity.defn
async def set_case_status(tenant: str, case_id: str, status: str, reason: str = "") -> None:
    with _conn() as c:
        c.execute(f"SET search_path TO tenant_{tenant}, public")
        c.execute(
            "UPDATE cases SET status=%s, status_reason=%s, updated_at=now() WHERE id=%s",
            (status, reason, case_id),
        )
        # hash-chained audit append
        row = c.execute(
            "SELECT COALESCE(MAX(hash),'GENESIS') FROM audit_log WHERE tenant=%s", (tenant,)
        ).fetchone()
        import hashlib, json
        payload = json.dumps({"case": case_id, "status": status, "reason": reason}, sort_keys=True)
        digest = hashlib.sha256((row[0] + payload).encode()).hexdigest()
        c.execute(
            "INSERT INTO audit_log (tenant, case_id, action, payload, prev_hash, hash)"
            " VALUES (%s,%s,'STATUS_CHANGE',%s,%s,%s)",
            (tenant, case_id, payload, row[0], digest),
        )


@activity.defn
async def post_ledger_transfer(tenant: str, transfer: dict) -> None:
    """Route escrow/fee postings through case-api (TigerBeetle owner)."""
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.post(
            f"/v1/tenants/{tenant}/fees/transfer",
            json=transfer,
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
        )
        resp.raise_for_status()


@activity.defn
async def request_lawful_reveal(case_id: str) -> None:
    """Ask the vault to unlock the offer window (BOTH_SUBMITTED / WINDOW_EXPIRED)."""
    async with httpx.AsyncClient(base_url=VAULT_URL, timeout=15) as client:
        # The vault independently verifies workflow state before any decrypt.
        resp = await client.post("/internal/mark-revealable", json={"case_id": case_id})
        resp.raise_for_status()


@activity.defn
async def notify_party(tenant: str, channel: str, to: str, template: str, data: dict) -> None:
    """Email/SMS via notifier; voice milestones place REAL outbound calls
    through the tenant-configured voice platform (getline.ai-style API)."""
    if channel == "voice":
        async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
            resp = await client.post(
                f"/v1/tenants/{tenant}/voice/outbound",
                json={"to": to, "case_number": data.get("case_number", ""),
                      "script": template, "variables": data},
                headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
            )
            resp.raise_for_status()
        return
    async with httpx.AsyncClient(base_url=NOTIFY_URL, timeout=15) as client:
        await client.post("/send", json={
            "tenant": tenant, "channel": channel, "to": to,
            "template": template, "data": data,
        })


@activity.defn
async def flag_cms_breach(tenant: str, case_id: str, clock: str, detail: str) -> None:
    """Statutory breach → CMS report + ESCALATION (supervisor notification chain).
    Also emits a tenant-outbox event so the breach reaches the lakehouse
    (bronze -> silver/sla_breaches feeds the CMS monthly report job)."""
    with _conn() as c:
        c.execute(
            "INSERT INTO public.sla_breaches (tenant, case_id, clock, detail) VALUES (%s,%s,%s,%s)",
            (tenant, case_id, clock, detail),
        )
        import json as _json
        c.execute(f"SET search_path TO tenant_{tenant}, public")
        c.execute(
            "INSERT INTO outbox (topic, key, payload) VALUES (%s,%s,%s::jsonb)",
            (f"idre.{tenant}.audit", case_id, _json.dumps({
                "type": "sla.breach", "tenant": tenant, "case_id": case_id,
                "clock": clock, "detail": detail})),
        )
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        await client.post(
            f"/v1/tenants/{tenant}/cases/{case_id}/escalate",
            json={"clock": clock, "detail": detail},
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
        )


@activity.defn
async def load_case_clocks(tenant: str) -> dict:
    """Fresh read of the tenant's program manifest clocks + features + holidays.

    Runs once at workflow start; the result is recorded in workflow history so
    replay is deterministic while the config itself stays hot-swappable for
    NEW cases (validate-on-write in case-api is the integrity gate).
    Absent/invalid manifest → empty blocks → NSA defaults (legacy behavior).
    """
    out: dict = {"clocks": [], "features": None, "holidays": []}
    with _conn() as c:
        row = c.execute(
            "SELECT config->'manifest' FROM public.program_rules"
            " WHERE tenant=%s AND config ? 'manifest'", (tenant,)
        ).fetchone()
        if row and isinstance(row[0], dict):
            m = row[0]
            out["clocks"] = m.get("clocks") or []
            out["features"] = m.get("features")  # None = no manifest features block
        rows = c.execute(
            "SELECT day::text FROM public.holidays WHERE tenant IN (%s,'*')", (tenant,)
        ).fetchall()
        out["holidays"] = [r[0] for r in rows]
    return out


@activity.defn
async def run_cms_monthly_report(tenant: str, month: str) -> str:
    """Trigger the Spark gold-zone CMS report job; returns the report object key."""
    async with httpx.AsyncClient(timeout=60) as client:
        resp = await client.post(
            os.environ.get("SPARK_SUBMIT_URL", "http://localhost:8091") + "/jobs/cms-report",
            json={"tenant": tenant, "month": month},
        )
        resp.raise_for_status()
        return resp.json()["object_key"]


# ACCT codes mirror case-api (cmd/server/main.go consts + payments.go).
ACCT_ESCROW, ACCT_ADMIN, ACCT_IDRE, ACCT_REFUND, ACCT_CLEARING = 1000, 3000, 4000, 5000, 6000


async def _ledger_balances(tenant: str) -> list[dict]:
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        resp = await client.get(
            f"/v1/tenants/{tenant}/internal/ledger/balances",
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
        )
        resp.raise_for_status()
        return resp.json()["accounts"]


@activity.defn
async def export_ledger_snapshot(tenant: str) -> int:
    """Snapshot TigerBeetle account balances into the tenant outbox so the
    lakehouse gold zone can build Mojaloop-style settlement_positions.
    Returns the number of accounts exported."""
    import json as _json
    accounts = await _ledger_balances(tenant)
    payload = {"type": "ledger.balance_snapshot", "tenant": tenant,
               "accounts": accounts}
    with _conn() as c:
        c.execute(f"SET search_path TO tenant_{tenant}, public")
        c.execute(
            "INSERT INTO outbox (topic, key, payload) VALUES (%s,%s,%s::jsonb)",
            (f"idre.{tenant}.fees", f"ledger-snapshot-{tenant}", _json.dumps(payload)),
        )
    return len(accounts)


@activity.defn
async def reconcile_ledger(tenant: str) -> dict:
    """Postgres <-> TigerBeetle reconciliation. Two checks:

    1. clearing_check: TB Stripe-clearing net == payments table net
       (PAID minus REFUNDED). Catches fail-open postPaymentLedger gaps.
    2. ledger_invariant: clearing net == escrow + admin + idre + refund nets.
       Every card dollar must land in exactly one liability bucket.

    Drift rows persist to public.ledger_reconciliation; nonzero drift raises
    an sla_breach + outbox event (lakehouse-visible, dashboard-alertable).
    """
    accounts = await _ledger_balances(tenant)
    net = {ACCT_ESCROW: 0, ACCT_ADMIN: 0, ACCT_IDRE: 0, ACCT_REFUND: 0, ACCT_CLEARING: 0}
    for a in accounts:
        if a["exists"]:
            net[a["code"]] = net.get(a["code"], 0) + a["credits_posted"] - a["debits_posted"]
    with _conn() as c:
        row = c.execute(
            "SELECT COALESCE(SUM(CASE WHEN status='PAID' THEN amount_cents"
            "                       WHEN status='REFUNDED' THEN -amount_cents END),0)"
            " FROM public.payments WHERE tenant=%s", (tenant,)
        ).fetchone()
        payments_net = row[0] if row else 0

        checks = [
            ("clearing_check", payments_net, net[ACCT_CLEARING]),
            ("ledger_invariant",
             net[ACCT_ESCROW] + net[ACCT_ADMIN] + net[ACCT_IDRE] + net[ACCT_REFUND],
             net[ACCT_CLEARING]),
        ]
        drift_found = False
        for name, expected, actual in checks:
            drift = actual - expected
            status = "OK" if drift == 0 else "DRIFT"
            c.execute(
                "INSERT INTO public.ledger_reconciliation"
                " (tenant, check_name, expected_cents, actual_cents, drift_cents, status)"
                " VALUES (%s,%s,%s,%s,%s,%s)",
                (tenant, name, expected, actual, drift, status),
            )
            if drift != 0:
                drift_found = True
                c.execute(
                    "INSERT INTO public.sla_breaches (tenant, case_id, clock, detail)"
                    " VALUES (%s,%s,%s,%s)",
                    (tenant, "-", "LEDGER_RECONCILIATION",
                     f"{name}: expected {expected} != ledger {actual} (drift {drift})"),
                )
        if drift_found:
            import json as _json
            c.execute(f"SET search_path TO tenant_{tenant}, public")
            c.execute(
                "INSERT INTO outbox (topic, key, payload) VALUES (%s,%s,%s::jsonb)",
                (f"idre.{tenant}.audit", "ledger-reconciliation", _json.dumps({
                    "type": "sla.breach", "tenant": tenant, "case_id": "-",
                    "clock": "LEDGER_RECONCILIATION",
                    "detail": "Postgres/TigerBeetle drift detected"})),
            )
    return {"tenant": tenant, "payments_net": payments_net, "ledger_net": net,
            "drift": drift_found}
