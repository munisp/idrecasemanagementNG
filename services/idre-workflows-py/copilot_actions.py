"""Copilot Phase 3 — bounded agentic action batches.

The Temporal side of the human gate: the workflow waits for a staff DECISION
signal (bounded by an approval TTL), then applies each proposed action one at
a time through worker-authenticated case-api endpoints. Temporal — not an
LLM loop — owns the orchestration: the wait survives restarts, the TTL is a
durable timer, and every execution step is retried + recorded.

Determinism rule: no I/O in the workflow body; all side effects are
activities. The batch row in public.copilot_action_batches is the UI's
source of truth; this workflow is its executor.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from datetime import timedelta

import httpx
import psycopg
from temporalio import activity, workflow
from temporalio.common import RetryPolicy

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")

MAX_ACTIONS = 5  # mirror of copilotMaxActions in case-api — defense in depth


@dataclass
class BatchInput:
    tenant: str
    case_id: str
    batch_id: str
    approval_ttl_seconds: int = 72 * 3600


@activity.defn
async def fetch_batch_action_count(tenant: str, batch_id: str) -> int:
    """How many actions the approved batch carries (capped server-side)."""
    with psycopg.connect(DSN, autocommit=True) as c:
        row = c.execute(
            "SELECT jsonb_array_length(actions) FROM public.copilot_action_batches"
            " WHERE tenant=%s AND id=%s AND status='APPROVED'",
            (tenant, batch_id),
        ).fetchone()
        return int(row[0]) if row else 0


@activity.defn
async def apply_copilot_action(tenant: str, batch_id: str, index: int) -> str:
    """Apply ONE action via case-api's worker-authenticated executor, which
    re-validates batch state + params and records the per-action result."""
    async with httpx.AsyncClient(base_url=CASE_API, timeout=30) as client:
        resp = await client.post(
            f"/v1/tenants/{tenant}/internal/copilot/actions/apply",
            json={"batch_id": batch_id, "index": index},
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
        )
        # A failed action is recorded on the batch row by case-api; keep
        # going with the rest of the batch rather than faulting the workflow.
        if resp.status_code >= 400:
            return f"action {index} failed: HTTP {resp.status_code}"
        return f"action {index}: {resp.json().get('result', 'applied')}"


@activity.defn
async def expire_copilot_batch(tenant: str, batch_id: str) -> None:
    """TTL elapsed with no human decision — mark EXPIRED (if still pending)
    and leave a timeline note on the case."""
    with psycopg.connect(DSN, autocommit=True) as c:
        row = c.execute(
            "UPDATE public.copilot_action_batches SET status='EXPIRED', updated_at=now()"
            " WHERE tenant=%s AND id=%s AND status='PENDING_APPROVAL' RETURNING case_id",
            (tenant, batch_id),
        ).fetchone()
        if row:
            c.execute(
                "INSERT INTO public.case_activities (tenant, case_id, type, body)"
                " VALUES (%s,%s,'COPILOT_BATCH_EXPIRED',%s)",
                (tenant, row[0], f"Copilot action batch {batch_id} expired with no decision"),
            )


@workflow.defn(name="CopilotActionBatchWorkflow")
class CopilotActionBatchWorkflow:
    """Waits for the human gate, then executes the approved batch.

    Signals:
      DECISION {"decision": "APPROVE"|"REJECT", "by": subject}
    """

    def __init__(self) -> None:
        self._decision: dict | None = None

    @workflow.signal(name="DECISION")
    def on_decision(self, data: dict) -> None:
        self._decision = data

    @workflow.run
    async def run(self, inp: BatchInput) -> str:
        ttl = timedelta(seconds=inp.approval_ttl_seconds or 72 * 3600)
        try:
            await workflow.wait_condition(lambda: self._decision is not None, timeout=ttl)
        except TimeoutError:
            await workflow.execute_activity(
                expire_copilot_batch, args=[inp.tenant, inp.batch_id],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "EXPIRED"

        decision = (self._decision or {}).get("decision", "REJECT")
        if decision != "APPROVE":
            return "REJECTED"

        n = await workflow.execute_activity(
            fetch_batch_action_count, args=[inp.tenant, inp.batch_id],
            start_to_close_timeout=timedelta(seconds=30),
        )
        results = []
        for i in range(min(n, MAX_ACTIONS)):
            res = await workflow.execute_activity(
                apply_copilot_action, args=[inp.tenant, inp.batch_id, i],
                start_to_close_timeout=timedelta(minutes=2),
                # Per-action retry: transient API/DB blips shouldn't fail the
                # batch; the executor is idempotent per index.
                retry_policy=RetryPolicy(maximum_attempts=3),
            )
            results.append(res)
        return "APPLIED: " + "; ".join(results) if results else "APPLIED: empty batch"
