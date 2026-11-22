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

- Detection scored via application logs, not Wazuh rules 100200–100261.
- ARTEX planner loop itself not executed (environment-blocked, above).
- T3/T8/T11 were partial in the first run; deepened 2026-11-23 (below).

## Second wave (2026-11-23): T3/T8/T11 deep run — 63/63 green

**T3 same-tenant IDOR (7 checks).** Positive control holds (staff reads the
seeded case); staff garbage/random ids answer clean 404s with no SQL leakage;
external-party probes confirmed the accessGuard `own-case` gate answers
**404, not 403** on unlinked cases — outside accounts cannot use responses
as a case-id oracle.

**T8 SSRF (3 checks + code audit).** `email-check` performs DNS on
user-supplied domains by design; verified it answers canned verdicts only —
no resolver data, IPs, or internal zone names in responses, including for
`localhost` targets. Code audit: every outbound URL in case-api is
config-sourced; no user-controlled fetch parameter exists.

**T11 PWA surface (12 checks).** No `.git`/`.env`/compose exposure, no
traversal, no shipped source maps, no hardcoded secrets, auth realm never
cached. One genuine finding, fixed this wave:

### F4 — MEDIUM: offline API cache survived logout

`portal/sw.js` caches every successful authenticated `/v1/` GET (case data)
in Cache Storage for offline reads, but `Auth.logout()` cleared only
sessionStorage — the `idre-api-v1` cache with case data persisted after
logout on a shared device. **Fix**: logout now purges all `idre-api*` caches
before the IdP redirect (shell cache stays — public assets). Also hardened:
the active `security_headers` snippet in `deploy/caddy/Caddyfile` now emits
HSTS (browsers ignore STS over plain HTTP per RFC 6797 §7.2, so the dev :80
vhost is unaffected; it engages the moment DOMAIN/TLS is live).

## Remediation (2026-11-23) — VERIFIED

Both findings are fixed and the suite re-ran **41/41 green** against the
fixed platform:

- **F1**: `scripts/migrate.sh` (idempotent, bounded fixpoint, extension-aware)
  now converges the full schema; `docker-compose.yml` first-boot runs it via
  `scripts/docker-initdb.sh`. Verified from an empty database: converged in
  round 5 of 8, `audit_log.org` and the full table set present. Existing
  deployments run `bash scripts/migrate.sh` once.
- **F2**: the tenancy middleware now checks a cached provisioned-tenant
  registry (`tenant_registry.go`, 60s TTL, fail-closed on DB error) and
  answers `503 {"error":"tenant not provisioned"}` before any handler runs.
  Verified live, plus `tenant_registry_test.go` pins the contract.
- Suite corrections from re-run (harness bugs, not platform): worker token
  travels as `Authorization: Bearer`, `/docs/seal` takes `{tenant,key,
  data_b64}`, storage list is `/objects/{bucket}`, and T7 now also accepts
  415 — the executable-extension blocklist refuses `.com` before the scanner
  stage, an even earlier fail-closed.
