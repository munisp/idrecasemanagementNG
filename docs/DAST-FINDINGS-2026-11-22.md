<!-- Author: Patrick Munis at newwave -->
# DAST Findings — Live Run 2026-11-22

Executes the deterministic checks of `deploy/artex/task-pack.md` against the
platform running **natively** (no docker available in the test environment).
The ARTEX planner/orchestrator itself was environment-blocked (no container
runtime, no root, RAM/CPU budget incompatible with an LLM-driven agent loop);
the task pack's checks were executed directly instead — every request below is
a real HTTP call against real services, and every expectation was verified
against the platform source before running.

## Environment (reference run)

| Component | How it ran |
|---|---|
| Postgres 16.2 | user-space `pgserver`, 50 `tenant_<st>` schemas + honeytokens seeded (`scripts/init-schemas.sql`, `scripts/seed-honeytokens.sql`) |
| case-api | native Go binary, `ADDR=127.0.0.1:8082`, `METRICS_PORT=9091` |
| vault-rs | native binary, `127.0.0.1:8081` |
| storage-rs | native binary (OpenDAL fs backend), `127.0.0.1:8094` |
| Auth | JWKS harness (`scripts/dast/jwks-*`) minting RS256 tokens — case-api performs full signature/issuer/tenant-claim verification; only the IdP is harnessed |
| Temporal | `scripts/dast/temporal-stub.py` (gRPC UNIMPLEMENTED — sdk-go v1.30.0 accepts it at dial; no workflow executions in this check set) |
| TigerBeetle | absent (client is lazy; no ledger ops in this check set) |
| Wazuh | **absent** — detection assertions made via case-api action logs, not SIEM rules |
| ClamAV | absent — used to prove the upload path fails closed |

## Results: 40 PASS / 1 FAIL (41 checks)

- **T0 preflight** — 3/3 PASS. healthz 200; `/metrics` absent from the API
  port (404) and served on the dedicated 9091 listener (200) — the
  availability-hardening change verified live.
- **T1 cross-tenant isolation** — 16/16 PASS. Cross-tenant list/read/
  advance/documents all 403; auditor read-only enforced (reads 200, write
  403); malformed tenant 400; path traversal blocked 400; unauthenticated 401;
  worker-token path works.
- **T2 token forgery** — 5/6 PASS. Expired, wrong-issuer, alg=none,
  wrong-key (real second RSA key), and garbage tokens all 401. One FAIL —
  see F2.
- **T4 internal service tokens** — 5/5 PASS. Vault and storage reject
  missing/wrong `X-Internal-Auth` with 401; correct token yields a real
  AES-GCM seal (200) and a real object write (201).
- **T5 honeytokens** — PASS. CANARY visible to a broad admin read; the touch
  is recorded by actionLog (log-verified; no Wazuh manager in sandbox).
- **T9 payments** — 3/3 PASS. Stripe webhook rejects bad/missing HMAC with
  401 (`bad signature`); checkout of unknown invoice is a clean 404.
- **T10 exposed surface** — PASS. healthz body carries no version/build
  detail; metrics unauthenticated-but-off-API-port by design (loopback/Cilium
  locked).
- **T6 injection** — 4/4 PASS. `sort`/`limit` SQLi probes silently ignored by
  the whitelist (200, no SQLSTATE leak in body); post-probe DB integrity
  confirmed (`CANARY-tx` intact).
- **T7 uploads** — PASS. With ClamAV absent, EICAR upload is refused
  (503) — the pipeline fails closed, nothing is stored.

## Findings

### F1 — HIGH: fresh-boot schema drift (deployment reproducibility)

A stack brought up exactly per `docker-compose.yml` (initdb loads only
`scripts/init-schemas.sql`) serves **500 on every case-list request**:
`column "org" of relation "audit_log" does not exist (SQLSTATE 42703)`.
The ~20 incremental scripts in `scripts/` (org_audit, audit_actor,
casemgmt-core, payments, …) are referenced by no compose wiring, no startup
migration, and no runbook ordering — case-api runs no migrations at boot.
In this run the schema only converged after applying scripts manually in
three dependency-ordering rounds, and `entity360.sql`/`db-optimization.sql`
additionally require the `pg_trgm` extension. **Recommendation:** add an
ordered migration runner (or an init wrapper that applies the numbered
scripts in dependency order) so a fresh environment converges without human
intervention.

### F2 — LOW: member of an unprovisioned tenant gets a 500

A valid token whose group claim names a tenant with no provisioned schema
(e.g. `/tenant/zz`) passes membership and fails at the query with a generic
500. No data crosses tenants (the schema does not exist) and the body leaks
nothing, but the platform should answer a clean 403/503. **Recommendation:**
verify schema presence (or a tenant registry lookup) in the tenancy
middleware and fail closed with a precise status.

### F3 — INFO: uppercase tenant codes are normalized

`TX` is lower-cased by the tenancy middleware and treated as `tx`. Verified
**not** a cross-tenant bypass (a non-member requesting `TX` still gets 403);
noted for canonical-input strictness only.

## Coverage caveats (honest)

- T3 IDOR covered only cross-tenant and not-found behavior; same-tenant
  object-level IDOR probes remain for the next run.
- T8 SSRF: no outbound-fetching endpoint was exercised in this environment.
- T11 PWA/static surface not probed (API-focused run).
- Detection scored via application logs, not Wazuh rules 100200–100261.
- ARTEX planner loop itself not executed (environment-blocked, above).

## Regression loop

Fix F1/F2, re-run `scripts/dast-checks.sh` to green, then keep the suite in
the pre-deploy cadence defined in `docs/RUNBOOK-artex-dast.md` (full pack
pre-deploy; T1+T4+T5 smoke after middleware waves).
