"""Temporal worker entrypoint: python worker.py

Also exposes a tiny HTTP listener used by Dapr bindings:
  POST /cms-monthly  — Dapr cron binding (deploy/dapr/components/cron-cms-monthly.yaml)
                       starts one CmsMonthlyReportWorkflow per tenant for the
                       month just ended. This is how the federal monthly report
                       fires without any hand-run job.
"""

import asyncio
import json
import os
import threading
from datetime import date
from http.server import BaseHTTPRequestHandler, HTTPServer

from temporalio.client import Client
from temporalio.worker import Worker

from activities import (
    set_case_status, post_ledger_transfer, request_lawful_reveal,
    notify_party, flag_cms_breach, run_cms_monthly_report,
)
from onboarding_activities import (
    export_ledger_snapshot, reconcile_ledger,
    set_application_status, check_ein_npi, check_idre_certification,
    check_state_requirements, provision_keycloak_account,
    provision_idre_ledger_accounts, send_portal_invite, record_onboarding_audit,
)
from workflows import (
    IdrCaseWorkflow, CmsMonthlyReportWorkflow, LedgerReconciliationWorkflow,
)
from onboarding import StakeholderOnboardingWorkflow, TenantOnboardingWorkflow


async def run_workers(client: Client) -> None:
    # Concurrency knobs: activities are I/O-bound (DB/HTTP) — run many in
    # flight per worker pod; scale pods horizontally beyond this.
    worker = Worker(
        client,
        task_queue="idre-cases",
        workflows=[IdrCaseWorkflow, CmsMonthlyReportWorkflow,
                   LedgerReconciliationWorkflow],
        activities=[
            set_case_status, post_ledger_transfer, request_lawful_reveal,
            notify_party, flag_cms_breach, run_cms_monthly_report,
            export_ledger_snapshot, reconcile_ledger,
        ],
        max_concurrent_activities=100,
        max_concurrent_workflow_tasks=200,
    )
    onboarding_worker = Worker(
        client,
        task_queue="idre-onboarding",
        workflows=[StakeholderOnboardingWorkflow, TenantOnboardingWorkflow],
        activities=[
            export_ledger_snapshot, reconcile_ledger,
    set_application_status, check_ein_npi, check_idre_certification,
            check_state_requirements, provision_keycloak_account,
            provision_idre_ledger_accounts, send_portal_invite, record_onboarding_audit,
        ],
        max_concurrent_activities=50,
        max_concurrent_workflow_tasks=100,
    )
    await asyncio.gather(worker.run(), onboarding_worker.run())


# ---- Dapr binding listener ---------------------------------------------------

ALL_STATES = ("al ak az ar ca co ct de fl ga hi id il in ia ks ky la me md ma mi "
              "mn ms mo mt ne nv nh nj nm ny nc nd oh ok or pa ri sc sd tn tx ut "
              "vt va wa wv wi wy").split()


def start_dapr_listener(client: Client) -> None:
    """Serve Dapr cron-binding invocations on DAPR_APP_PORT (default 9100)."""

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802 — stdlib naming
            path = self.path.rstrip("/")
            if path == "/ledger-reconcile":
                started = 0
                today = date.today().isoformat()
                for tenant in ALL_STATES:
                    asyncio.run_coroutine_threadsafe(
                        client.start_workflow(
                            "LedgerReconciliationWorkflow",
                            {"tenant": tenant},
                            id=f"RECON-{tenant}-{today}",
                            task_queue="idre-cases",
                        ),
                        _MAIN_LOOP,
                    )
                    started += 1
                body = json.dumps({"date": today, "workflows_started": started}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if path != "/cms-monthly":
                self.send_response(404)
                self.end_headers()
                return
            today = date.today()
            year, mon = (today.year, today.month - 1) if today.month > 1 else (today.year - 1, 12)
            month_str = f"{year}-{mon:02d}"
            started = 0
            for tenant in ALL_STATES:
                asyncio.run_coroutine_threadsafe(
                    client.start_workflow(
                        "CmsMonthlyReportWorkflow",
                        {"tenant": tenant, "month": month_str},
                        id=f"CMS-{tenant}-{month_str}",
                        task_queue="idre-cases",
                    ),
                    _MAIN_LOOP,
                )
                started += 1
            body = json.dumps({"month": month_str, "workflows_started": started}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args) -> None:  # quiet
            pass

    port = int(os.environ.get("DAPR_APP_PORT", "9100"))
    threading.Thread(target=lambda: HTTPServer(("0.0.0.0", port), Handler).serve_forever(),
                     daemon=True).start()


_MAIN_LOOP: asyncio.AbstractEventLoop | None = None


async def main() -> None:
    global _MAIN_LOOP
    _MAIN_LOOP = asyncio.get_running_loop()
    client = await Client.connect(
        os.environ.get("TEMPORAL_HOST", "localhost:7233"),
        namespace=os.environ.get("TEMPORAL_NAMESPACE", "idre"),
    )
    start_dapr_listener(client)
    await run_workers(client)


if __name__ == "__main__":
    asyncio.run(main())
