# Author: Patrick Munis at newwave
"""Onboarding activities: verification + provisioning side effects."""

from __future__ import annotations

import os

import httpx
import psycopg
from temporalio import activity

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
KEYCLOAK = os.environ.get("KEYCLOAK_URL", "http://localhost:8085")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")


@activity.defn
async def set_application_status(app_id: str, status: str, reason: str = "") -> None:
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            "UPDATE public.stakeholder_applications SET status=%s, status_reason=%s,"
            " updated_at=now() WHERE id=%s",
            (status, reason, app_id),
        )


@activity.defn
async def check_ein_npi(ein: str, npi: str) -> bool:
    """EIN format check + NPPES NPI registry lookup with OFFLINE FALLBACK:
    live NPPES first (5s timeout); on any outage, consult the local
    public.npi_cache (refreshed on every successful live lookup, seedable
    from the weekly NPPES bulk file) and mark the verification degraded
    in the audit trail instead of blocking onboarding."""
    if ein and len(ein.replace("-", "")) != 9:
        return False
    if not npi:
        return True  # NPI only required for provider orgs (checked upstream)

    # 1) live NPPES
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(
                "https://npiregistry.cms.hhs.gov/api/",
                params={"version": "2.1", "number": npi},
            )
            ok = resp.status_code == 200 and resp.json().get("result_count", 0) >= 1
            with psycopg.connect(DSN, autocommit=True) as c:
                c.execute(
                    "INSERT INTO public.npi_cache (npi, valid, checked_at)"
                    " VALUES (%s,%s,now())"
                    " ON CONFLICT (npi) DO UPDATE SET valid=EXCLUDED.valid,"
                    " checked_at=now(), source='live'",
                    (npi, ok),
                )
            return ok
    except (httpx.HTTPError, OSError):
        pass  # fall through to cache

    # 2) offline fallback: cache hit decides; cache miss = soft-pass + flag
    with psycopg.connect(DSN, autocommit=True) as c:
        row = c.execute(
            "SELECT valid FROM public.npi_cache WHERE npi=%s", (npi,)
        ).fetchone()
        degraded_ok = bool(row[0]) if row else True   # unknown NPI: don't block
        c.execute(
            "INSERT INTO public.audit_log (tenant, case_id, action, payload, prev_hash, hash)"
            " VALUES ('platform', %s, 'NPI_VERIFICATION_DEGRADED', %s, '', %s)",
            (npi, f"cache_hit={row is not None}", f"npi-degraded:{npi}"),
        )
    return degraded_ok


@activity.defn
async def check_idre_certification(legal_name: str, stype: str) -> bool:
    """IDRE entities must be CMS-certified; others skip this gate."""
    if stype != "IDRE_ENTITY":
        return True
    with psycopg.connect(DSN) as c:
        row = c.execute(
            "SELECT certified FROM public.idre_directory WHERE legal_name ILIKE %s",
            (legal_name,),
        ).fetchone()
    return bool(row and row[0])


@activity.defn
async def check_state_requirements(tenant: str, stype: str, payload: dict) -> dict:
    """Per-state onboarding requirements matrix (research-driven, admin-editable)."""
    with psycopg.connect(DSN) as c:
        row = c.execute(
            "SELECT onboarding_requirements FROM public.state_config WHERE tenant=%s",
            (tenant,),
        ).fetchone()
    reqs = (row[0] or {}).get(stype, []) if row else []
    missing = [r for r in reqs if r not in payload]
    return {"ok": not missing, "missing": missing}


@activity.defn
async def provision_keycloak_account(tenant: str, stype: str, inp: dict) -> None:
    """Create the user, assign realm role + /tenant/<state> group."""
    role = {
        "IDRE_ENTITY": "ARBITRATOR", "PROVIDER_ORG": "PARTY",
        "PAYER_ORG": "PARTY", "STATE_AUDITOR_ORG": "STATE_AUDITOR",
        "ADMIN_STAFF": "CASE_MANAGER",
    }.get(stype, "PARTY")
    async with httpx.AsyncClient(base_url=KEYCLOAK, timeout=30) as client:
        # admin token from service secret (Dapr secret store in k8s)
        tok = (await client.post("/realms/master/protocol/openid-connect/token", data={
            "grant_type": "password", "client_id": "admin-cli",
            "username": os.environ["KC_ADMIN"], "password": os.environ["KC_ADMIN_PASSWORD"],
        })).json()["access_token"]
        h = {"Authorization": f"Bearer {tok}"}
        await client.post("/admin/realms/idre/users", headers=h, json={
            "username": inp.get("contact_email", f"{stype.lower()}-{inp['application_id']}"),
            "enabled": True, "groups": [f"/tenant/{tenant}"],
            "realmRoles": [role], "requiredActions": ["UPDATE_PASSWORD", "VERIFY_EMAIL"],
        })


@activity.defn
async def provision_idre_ledger_accounts(tenant: str, app_id: str, stype: str) -> None:
    """IDRE entities get settlement accounts on the tenant TigerBeetle ledger."""
    if stype != "IDRE_ENTITY":
        return
    async with httpx.AsyncClient(base_url=CASE_API, timeout=15) as client:
        await client.post(f"/v1/tenants/{tenant}/fees/transfer", json={
            "case_id": f"onboarding-{app_id}", "kind": "SETTLEMENT",
            "party_id": app_id, "amount_cents": 0, "post_kind": "POST",
        }, headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"})


@activity.defn
async def send_portal_invite(tenant: str, inp: dict) -> None:
    async with httpx.AsyncClient(
        base_url=os.environ.get("NOTIFY_URL", "http://localhost:8090"), timeout=15,
    ) as client:
        await client.post("/send", json={
            "tenant": tenant, "channel": "email", "to": inp.get("contact_email"),
            "template": "onboarding_activated", "data": {"legal_name": inp["legal_name"]},
        })


@activity.defn
async def record_onboarding_audit(app_id: str, action: str, detail: str) -> None:
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            "INSERT INTO public.audit_log (tenant, case_id, action, payload, prev_hash, hash)"
            " VALUES ('platform', %s, %s, %s, '', %s)",
            (app_id, action, detail, f"{action}:{app_id}"),
        )
