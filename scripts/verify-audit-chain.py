#!/usr/bin/env python3
"""Nightly audit hash-chain verification (go-live checklist 3.2).

public.audit_log rows are hash-chained: hash = sha256(prev_hash || payload),
payload stored as the exact canonical JSON text written by the producers
(activities.py / onboarding_activities.py use json.dumps(..., sort_keys=True)).
Recompute per tenant in id order; any mismatch = tamper or producer bug.
Exits nonzero on mismatch (CronJob failure -> AuditChainBroken alert).

Usage: python3 scripts/verify-audit-chain.py   (DATABASE_URL env)
"""

import hashlib
import os
import sys

import psycopg

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")


def main() -> int:
    total, bad = 0, 0
    with psycopg.connect(DSN, autocommit=True) as conn:
        rows = conn.execute(
            "SELECT tenant, id, case_id, payload, prev_hash, hash"
            " FROM public.audit_log ORDER BY tenant, id"
        ).fetchall()
    current_tenant, checked, mism = None, 0, 0
    for tenant, _id, case_id, payload, prev_hash, stored in rows:
        if tenant != current_tenant:
            if current_tenant is not None:
                print(f"tenant={current_tenant} rows={checked} mismatches={mism}", flush=True)
            current_tenant, checked, mism = tenant, 0, 0
        checked += 1
        total += 1
        expect = hashlib.sha256((prev_hash + payload).encode()).hexdigest()
        if expect != stored:
            mism += 1
            bad += 1
            print(f"MISMATCH tenant={tenant} id={_id} case={case_id}", flush=True)
    if current_tenant is not None:
        print(f"tenant={current_tenant} rows={checked} mismatches={mism}", flush=True)
    print(f"TOTAL rows={total} mismatches={bad}", flush=True)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
