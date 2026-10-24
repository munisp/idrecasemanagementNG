# Author: Patrick Munis at newwave
"""Onboarding workflows — durable, per-tenant stakeholder onboarding.

StakeholderOnboardingWorkflow: one per application (IDRE entity, provider org,
payer org, auditor org, admin staff). Gates: document verification (fed by the
doc-intel pipeline), compliance checks (EIN/NPI, CMS IDRE certification, COI
attestation, state-specific requirements), role-based human approval, then
automated provisioning (Keycloak account/group, TigerBeetle accounts for IDREs,
portal invite).

TenantOnboardingWorkflow: brings a whole state tenant live (schema, ledger,
topics, Keycloak group, config, smoke checks, go-live).
"""

from __future__ import annotations

from datetime import timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from onboarding_activities import (
        set_application_status,
        check_ein_npi,
        check_idre_certification,
        check_state_requirements,
        provision_keycloak_account,
        provision_idre_ledger_accounts,
        send_portal_invite,
        record_onboarding_audit,
    )

APPLICATION_SLA_DAYS = 10          # target decision window; breach -> reminder
REMINDER_INTERVAL_DAYS = 3


@workflow.defn(name="StakeholderOnboardingWorkflow")
class StakeholderOnboardingWorkflow:
    def __init__(self) -> None:
        self.decision: str | None = None
        self.decision_reason = ""
        self.docs_verified = False
        self.doc_findings: list[dict] = []

    @workflow.signal(name="DECISION")
    def on_decision(self, data: dict) -> None:
        self.decision = data["decision"]
        self.decision_reason = data.get("reason", "")

    @workflow.signal(name="DOCS_VERIFIED")
    def on_docs(self, data: dict) -> None:
        # Fed by doc-intel: required onboarding documents analyzed clean.
        self.docs_verified = data.get("clean", False)
        self.doc_findings = data.get("findings", [])

    @workflow.run
    async def run(self, inp: dict) -> str:
        tenant, app_id, stype = inp["tenant"], inp["application_id"], inp["type"]

        await workflow.execute_activity(
            set_application_status, args=[app_id, "VERIFYING", "automated checks started"],
            start_to_close_timeout=timedelta(seconds=30),
        )

        # --- automated compliance gates (parallel, deterministic gather) ------
        import asyncio
        ein_ok, cert_ok, state_ok = await asyncio.gather(
            workflow.execute_activity(
                check_ein_npi, args=[inp.get("ein", ""), inp.get("npi", "")],
                start_to_close_timeout=timedelta(seconds=60),
            ),
            workflow.execute_activity(
                check_idre_certification, args=[inp["legal_name"], stype],
                start_to_close_timeout=timedelta(seconds=60),
            ),
            workflow.execute_activity(
                check_state_requirements, args=[tenant, stype, inp.get("payload", {})],
                start_to_close_timeout=timedelta(seconds=30),
            ),
        )
        if not (ein_ok and cert_ok and state_ok["ok"]):
            await workflow.execute_activity(
                set_application_status,
                args=[app_id, "REJECTED_AUTO",
                      f"ein={ein_ok} cert={cert_ok} state={state_ok.get('missing')}"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "REJECTED_AUTO"

        # --- document verification gate (onboarding docs analyzed by doc-intel) --
        await workflow.execute_activity(
            set_application_status, args=[app_id, "PENDING_DOCS", "awaiting document analysis"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        got_docs = await workflow.wait_condition(
            lambda: self.docs_verified, timeout=timedelta(days=APPLICATION_SLA_DAYS),
        )
        if not got_docs:
            await workflow.execute_activity(
                set_application_status, args=[app_id, "EXPIRED", "documents not verified in time"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "EXPIRED"

        # --- human approval (authority matrix enforced in case-api + here) ------
        await workflow.execute_activity(
            set_application_status, args=[app_id, "PENDING_APPROVAL", ""],
            start_to_close_timeout=timedelta(seconds=30),
        )
        for _ in range(APPLICATION_SLA_DAYS // REMINDER_INTERVAL_DAYS):
            decided = await workflow.wait_condition(
                lambda: self.decision is not None,
                timeout=timedelta(days=REMINDER_INTERVAL_DAYS),
            )
            if decided:
                break
            await workflow.execute_activity(
                record_onboarding_audit, args=[app_id, "REMINDER_SENT", ""],
                start_to_close_timeout=timedelta(seconds=30),
            )
        if self.decision is None:
            await workflow.execute_activity(
                set_application_status, args=[app_id, "EXPIRED", "no decision in SLA"],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "EXPIRED"

        if self.decision == "REJECT":
            await workflow.execute_activity(
                set_application_status, args=[app_id, "REJECTED", self.decision_reason],
                start_to_close_timeout=timedelta(seconds=30),
            )
            return "REJECTED"

        # --- provisioning --------------------------------------------------------
        await asyncio.gather(
            workflow.execute_activity(
                provision_keycloak_account, args=[tenant, stype, inp],
                start_to_close_timeout=timedelta(seconds=60),
            ),
            workflow.execute_activity(
                provision_idre_ledger_accounts, args=[tenant, app_id, stype],
                start_to_close_timeout=timedelta(seconds=60),
            ),
        )
        await workflow.execute_activity(
            send_portal_invite, args=[tenant, inp],
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.execute_activity(
            set_application_status, args=[app_id, "ACTIVE", "provisioned"],
            start_to_close_timeout=timedelta(seconds=30),
        )
        return "ACTIVE"


@workflow.defn(name="TenantOnboardingWorkflow")
class TenantOnboardingWorkflow:
    """Brings a state tenant live: schema, ledger, topics, identity group,
    config load, smoke checks. Mirrors scripts/provision-tenant.sh but durable
    and auditable — used for ongoing operations, not just bootstrap."""

    @workflow.run
    async def run(self, inp: dict) -> str:
        tenant = inp["tenant"]
        steps = [
            ("provision_schema", "scripts/provision-tenant.sh (schema+ledger+topics+group)"),
            ("load_state_config", "SSL program, citations, holidays, fee schedule"),
            ("seed_idre_directory", "certified IDRE entities for this state"),
            ("smoke_checks", "initiate+sweep a synthetic dispute end-to-end"),
        ]
        for step, detail in steps:
            await workflow.execute_activity(
                record_onboarding_audit, args=[tenant, f"TENANT_STEP:{step}", detail],
                start_to_close_timeout=timedelta(minutes=5),
            )
        return "TENANT_LIVE"
