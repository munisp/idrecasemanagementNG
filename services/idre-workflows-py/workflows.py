"""Temporal workflows — the statutory lifecycle of a dispute case.

One long-running workflow per case. Signals = business events, timers =
statutory clocks. Phase durations are resolved from the tenant's Program
Manifest (clocks + features, loaded once via activity and recorded in
history, so replay stays deterministic); absent manifest → NSA defaults,
so legacy healthcare tenants behave exactly as before. All side effects
are activities; this module must stay deterministic (replay-safe).
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import date, timedelta

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    import case_clocks
    from activities import (
        set_case_status, post_ledger_transfer, request_lawful_reveal,
        notify_party, flag_cms_breach, run_cms_monthly_report,
        load_case_clocks,
        export_ledger_snapshot, reconcile_ledger,
    )

ACT_TIMEOUT = timedelta(seconds=30)


@dataclass
class CaseInput:
    tenant: str
    case_id: str
    case_number: str
    open_negotiation_end: str  # YYYY-MM-DD ("" if the program has no negotiation phase)
    plan_type: str             # FULLY_INSURED | SELF_FUNDED (healthcare; opaque elsewhere)


@workflow.defn(name="IdrCaseWorkflow")
class IdrCaseWorkflow:
    def __init__(self) -> None:
        self.response_filed = False
        self.offers_submitted: set[str] = set()
        self.fees_paid: set[str] = set()
        self.selection_finalized = False
        self.determination_issued = False
        self.payment_recorded = False
        self.settled_or_withdrawn = False

    # ---- signals -----------------------------------------------------------
    @workflow.signal(name="RESPONSE_FILED")
    def on_response(self, data: dict) -> None:
        self.response_filed = True

    @workflow.signal(name="OFFER_SUBMITTED")
    def on_offer(self, data: dict) -> None:
        self.offers_submitted.add(data["party_id"])

    @workflow.signal(name="FEES_PAID")
    def on_fees(self, data: dict) -> None:
        self.fees_paid.add(data["party_id"])

    @workflow.signal(name="SELECTION_FINALIZED")
    def on_selection(self, data: dict) -> None:
        self.selection_finalized = True

    @workflow.signal(name="DETERMINATION_ISSUED")
    def on_determination(self, data: dict) -> None:
        self.determination_issued = True

    @workflow.signal(name="PAYMENT_RECORDED")
    def on_payment(self, data: dict) -> None:
        self.payment_recorded = True

    @workflow.signal(name="SETTLED_OR_WITHDRAWN")
    def on_settled(self, data: dict) -> None:
        self.settled_or_withdrawn = True

    # ---- main --------------------------------------------------------------
    @workflow.run
    async def run(self, inp: CaseInput) -> str:
        tenant = inp.tenant
        case = inp.case_id

        # Manifest clocks + features + holidays — loaded once, then fixed in
        # history. Missing manifest → NSA defaults (legacy tenants unchanged).
        cfg = await workflow.execute_activity(
            load_case_clocks, args=[tenant], start_to_close_timeout=ACT_TIMEOUT,
        )
        clocks = case_clocks.resolve(cfg.get("clocks"))
        holidays = {date.fromisoformat(d) for d in cfg.get("holidays", [])}
        feats = cfg.get("features")  # None = legacy tenant, everything on
        sealed_offers = case_clocks.feature(feats, "sealed_offers")
        negotiation_window = case_clocks.feature(feats, "negotiation_window")

        def window(clock_name: str) -> timedelta:
            """Calendar upper-bound timeout for a manifest clock, from today."""
            days, day_type = clocks[clock_name]
            end = case_clocks.deadline(workflow.now().date(), days, day_type, holidays)
            return timedelta(days=(end - workflow.now().date()).days + 1)

        # --- Pre-phase: open negotiation still running ------------------------
        # Only for programs whose manifest enables negotiation_window. The case
        # may be registered before the negotiation period ends; the workflow
        # waits durably and OPENS THE CASE AUTOMATICALLY at expiry.
        neg_end = date.fromisoformat(inp.open_negotiation_end) \
            if inp.open_negotiation_end else workflow.now().date()
        today = workflow.now().date()
        if negotiation_window and neg_end > today:
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "NEGOTIATION_TRACKED",
                                       "Registered; awaiting end of negotiation window"],
                start_to_close_timeout=ACT_TIMEOUT,
            )
            remaining = (neg_end - today).days + 1
            try:
                await workflow.wait_condition(
                    lambda: self.settled_or_withdrawn,
                    timeout=timedelta(days=remaining),
                )
            except TimeoutError:
                pass  # negotiation window expired → auto-open below
            if self.settled_or_withdrawn:
                return await self._close_early(tenant, case, "SETTLED_IN_NEGOTIATION")
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "INITIATED",
                                       "Negotiation window ended — case auto-opened by statutory timer"],
                start_to_close_timeout=ACT_TIMEOUT,
            )
        else:
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "INITIATED", "Case initiated"],
                start_to_close_timeout=ACT_TIMEOUT,
            )

        # --- Non-initiating party response (response_window clock) ----------
        try:
            await workflow.wait_condition(
                lambda: self.response_filed or self.settled_or_withdrawn,
                timeout=window("response_window"),
            )
        except TimeoutError:
            pass  # response optional; clock still advances per rule
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_PRE_ELIGIBILITY")

        # --- Offer window (offer_window clock) — only for sealed-offer programs
        if sealed_offers:
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "OFFER_WINDOW_OPEN", ""],
                start_to_close_timeout=ACT_TIMEOUT,
            )
            await workflow.wait_condition(
                lambda: len(self.offers_submitted) >= 2 or self.settled_or_withdrawn,
                timeout=window("offer_window"),
            )
            if self.settled_or_withdrawn:
                return await self._close_early(tenant, case, "SETTLEMENT_PRE_ELIGIBILITY")

            reveal_reason = "BOTH_SUBMITTED" if len(self.offers_submitted) >= 2 else "WINDOW_EXPIRED"
            await workflow.execute_activity(
                request_lawful_reveal, args=[case],
                start_to_close_timeout=ACT_TIMEOUT,
            )
            await workflow.execute_activity(
                set_case_status, args=[tenant, case, "OFFERS_REVEALED", reveal_reason],
                start_to_close_timeout=ACT_TIMEOUT,
            )

        # --- Neutral selection (selection_window clock) ----------------------
        await workflow.wait_condition(
            lambda: self.selection_finalized or self.settled_or_withdrawn,
            timeout=window("selection_window"),
        )
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_POST_ELIGIBILITY")

        # --- Determination (determination_window clock) -----------------------
        determined = await workflow.wait_condition(
            lambda: self.determination_issued or self.settled_or_withdrawn,
            timeout=window("determination_window"),
        )
        if self.settled_or_withdrawn:
            return await self._close_early(tenant, case, "SETTLEMENT_POST_ELIGIBILITY")
        if not determined:
            d_days, d_type = clocks["determination_window"]
            await workflow.execute_activity(
                flag_cms_breach,
                args=[tenant, case,
                      f"DETERMINATION_{d_days}{'BD' if d_type == 'business' else 'CD'}",
                      "Neutral exceeded statutory determination window"],
                start_to_close_timeout=ACT_TIMEOUT,
            )
            await workflow.wait_condition(lambda: self.determination_issued)

        # --- settle neutral fees via TigerBeetle (double-entry) ---------------
        await workflow.execute_activity(
            post_ledger_transfer,
            args=[tenant, {"case_id": case, "kind": "IDRE_FEE_RESERVE",
                           "party_id": "both", "amount_cents": 0, "post_kind": "POST"}],
            start_to_close_timeout=ACT_TIMEOUT,
        )

        # --- Payment by non-prevailing party (payment_window clock) -----------
        paid = await workflow.wait_condition(
            lambda: self.payment_recorded, timeout=window("payment_window"),
        )
        if not paid:
            p_days, p_type = clocks["payment_window"]
            await workflow.execute_activity(
                flag_cms_breach,
                args=[tenant, case,
                      f"PAYMENT_{p_days}{'BD' if p_type == 'business' else 'CD'}",
                      "prevailing party unpaid"],
                start_to_close_timeout=ACT_TIMEOUT,
            )
            await workflow.wait_condition(lambda: self.payment_recorded)

        await workflow.execute_activity(
            set_case_status, args=[tenant, case, "CLOSED_PAID", ""],
            start_to_close_timeout=ACT_TIMEOUT,
        )
        await workflow.execute_activity(
            notify_party,
            args=[tenant, "voice", "both", "case_closed", {"case_number": inp.case_number}],
            start_to_close_timeout=ACT_TIMEOUT,
        )
        return "CLOSED_PAID"

    async def _close_early(self, tenant: str, case: str, status: str) -> str:
        await workflow.execute_activity(
            set_case_status, args=[tenant, case, status, ""],
            start_to_close_timeout=ACT_TIMEOUT,
        )
        # refund/void escrow reservations per settlement fee rules
        await workflow.execute_activity(
            post_ledger_transfer,
            args=[tenant, {"case_id": case, "kind": "REFUND", "party_id": "both",
                           "amount_cents": 0, "post_kind": "VOID"}],
            start_to_close_timeout=ACT_TIMEOUT,
        )
        return status


@workflow.defn(name="CmsMonthlyReportWorkflow")
class CmsMonthlyReportWorkflow:
    """Cron-scheduled per tenant: regulator monthly report after month end."""

    @workflow.run
    async def run(self, args: dict) -> str:
        key = await workflow.execute_activity(
            run_cms_monthly_report, args=[args["tenant"], args["month"]],
            start_to_close_timeout=timedelta(minutes=10),
        )
        return key


@workflow.defn(name="LedgerReconciliationWorkflow")
class LedgerReconciliationWorkflow:
    """Daily per tenant: export TB balances to the lakehouse, then reconcile
    Postgres payments against ledger balances. Drift -> breach + alert."""

    @workflow.run
    async def run(self, args: dict) -> dict:
        tenant = args["tenant"]
        await workflow.execute_activity(
            export_ledger_snapshot, args=[tenant], start_to_close_timeout=timedelta(minutes=2),
        )
        result = await workflow.execute_activity(
            reconcile_ledger, args=[tenant], start_to_close_timeout=timedelta(minutes=2),
        )
        return result
