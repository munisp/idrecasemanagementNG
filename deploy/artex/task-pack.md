<!-- Author: Patrick Munis at newwave -->
# ARTEX Task Pack — IDRE Platform DAST

Pre-configured ARTEX tasks for testing this platform. Create each as a task
in the ARTEX UI (**Tasks → New task**), paste the goal text verbatim, attach
the listed test assets (System → Test assets), and apply the scope policy
below to every task. Operating procedure: `docs/RUNBOOK-artex-dast.md`.

## Global scope policy (apply to EVERY task)

- **Allow:** `localhost`, `127.0.0.1`, and the compose service names
  `apisix`, `case-api`, `keycloak`, `portal`, `vault`, `storage`
- **Deny:** everything else — no public IPs, no domain names, no `*.gov`
- **Approval gates:** ON for any mutating or bulk action (default)
- **Targets of record:** edge `http://localhost` (APISIX via Caddy) and the
  PWA at `http://localhost:3001`. Direct service ports (`case-api:8080`)
  only where a task says so.

## Test assets (created by `scripts/seed-artex-test-users.sh`)

| Asset | Role | Reach | Password env |
|-------|------|-------|--------------|
| `tx-tester` | CASE_MANAGER | read+write, tenant tx only | `TX_TESTER_PW` |
| `ny-tester` | CASE_MANAGER | read+write, tenant ny only | `NY_TESTER_PW` |
| `auditor-tester` | STATE_AUDITOR | read-only, all tenants | `AUDITOR_TESTER_PW` |
| `admin-tester` | PLATFORM_ADMIN | read+write, all tenants | `ADMIN_TESTER_PW` |

Tokens: `POST /auth/realms/idre/protocol/openid-connect/token`,
`grant_type=password`, `client_id=case-portal`.

---

### T1 — Cross-tenant isolation (the crown jewel)
**Goal:** "Authenticate as tx-tester and as ny-tester. As ny-tester, attempt
to read, list, modify, or delete tx-tester's cases, documents, and offers by
every method available: direct ID access, list filtering, search, document
download URLs, offer reveal, telemetry endpoints. Prove whether any tx data
is reachable from the ny identity."
**Assets:** tx-tester, ny-tester
**Expected defense:** 403 everywhere; empty lists (RLS), never error leaks.
**Detection:** Wazuh 100211 (cross-tenant probing) must fire.
**If found:** CRITICAL — stop, fix before any further testing.

### T2 — Token forgery and session attacks
**Goal:** "Attempt to access the API with: expired tokens, tokens signed
with wrong keys, algorithm-confusion variants (alg=none, RS256->HS256),
tokens for wrong issuer/audience, tampered group claims (add /tenant/zz or
/tenant/../), and replayed refresh tokens beyond the reuse limit."
**Expected defense:** 401 everywhere; realm hardening (300s tokens, reuse=0).
**If found:** CRITICAL.

### T3 — IDOR / BOLA inside one tenant
**Goal:** "As tx-tester, access cases, documents, offers, invoices, and
party data belonging to OTHER users within tenant tx by manipulating IDs
(UUIDs, sequential guesses, other case IDs observed in lists)."
**Expected defense:** Permify ReBAC object-level checks deny access.
**Known artifact:** when `PERMIFY_URL` is unset, ReBAC is in dev-bypass —
findings here are dev-mode artifacts; re-verify with Permify enabled.
**If found with Permify on:** HIGH.

### T4 — Internal service token enforcement
**Goal:** "Call the vault (vault:8081 /docs/seal, /docs/open, /seal) and the
storage gateway (storage:8094 /objects/*, /upload-begin/*) directly, without
X-Internal-Auth, with a wrong token, and with a token from a different
service. Attempt the same from outside the platform network."
**Expected defense:** 401; no route works tokenless.
**Detection:** Wazuh 100260/100261 (internal-token failures) must fire.
**If found:** CRITICAL (crypto boundary breach).

### T5 — Honeytoken tripwire validation
**Goal:** "Enumerate cases across tenants and read any case whose number
begins with CANARY-. Retrieve the associated documents and object keys."
**Assets:** admin-tester (broadest legitimate reach)
**Expected defense:** admin can read them (they look real) — that is the
point: ANY access is hostile.
**Detection:** Wazuh 100200 must fire at level 15 on every touch. If ARTEX
touched canaries and Wazuh stayed silent, the detection is broken — that
is the finding.

### T6 — Injection across the API surface
**Goal:** "Probe every list/search/filter parameter, the analytics query
endpoint, case detail JSON fields, and the document-intel pipeline for SQL
injection (error-based, blind, time-based), NoSQL/operator injection, and
second-order injection via stored case fields rendered elsewhere."
**Assets:** tx-tester
**Expected defense:** parameterized queries; analytics read-only gate
(`validate_readonly_sql`) rejects writes; row caps hold.
**If found:** CRITICAL (SQLi) / HIGH (second-order).

### T7 — Upload pipeline abuse
**Goal:** "Upload through the document endpoint: EICAR and polyglot files
(PDF+PE, image+script), extension/MIME mismatches, oversized bodies beyond
16M, zip bombs, and filenames containing path traversal sequences. Verify
each outcome and whether the object store ever holds unscanned content."
**Assets:** tx-tester
**Expected defense:** ClamAV scans fail-closed; body limit rejects;
filenames normalized; traversal impossible through the OpenDAL gateway.
**Detection:** Wazuh 100250/100251 (malware) on EICAR.
**If found:** HIGH (unscanned storage) / MEDIUM (limit bypass).

### T8 — SSRF and URL-driven fetch
**Goal:** "Find every parameter that accepts a URL or host (provider
callbacks, webhook URLs, import endpoints, document source URLs) and attempt
SSRF toward link-local metadata (169.254.169.254), loopback services
(vault:8081, postgres:5432, prometheus:9090), and the ARTEX host itself."
**Expected defense:** no user-controlled URL fetching; egress locked to
stripe/mail by Cilium in k8s shape.
**If found:** HIGH.

### T9 — Payment flow attacks
**Goal:** "Attempt: Stripe webhook posts with invalid/missing signatures,
replayed webhook events, checkout amount manipulation, race on fee
transfer, negative/refund-abuse amounts, and provider=mojaloop routing when
the adapter is unconfigured."
**Assets:** tx-tester
**Expected defense:** signature verification rejects; amounts are
server-side; TigerBeetle double-entry rejects imbalance; unconfigured
provider returns 400.
**If found:** CRITICAL (ledger integrity).

### T10 — Exposed surface and information disclosure
**Goal:** "Enumerate every reachable port and endpoint on the platform
network from an unauthenticated position: /metrics (9091), /healthz,
Prometheus (9090), Grafana (3000), Alertmanager (9093), MinIO console,
Keycloak admin, Temporal, OpenSearch. Document what each leaks without
credentials."
**Expected defense:** /metrics unreachable (9091 not published); Grafana
loopback-only; no version/stack detail beyond healthz 200.
**If found:** MEDIUM (info leak) / HIGH (reachable admin plane).

### T11 — PWA client-side attacks
**Goal:** "Through the portal at :3001 (drive it via the ARTEX browser
extension / MITM proxy): stored and DOM XSS in case fields, party names,
and document metadata; CSRF on state-changing API calls; JWT storage and
exfiltration paths; clickjacking on the payment flow."
**Assets:** tx-tester
**Expected defense:** output-escaped rendering, CSP denies inline,
SameSite/origin checks on mutations.
**If found:** HIGH (stored XSS in a case-management UI).

---

## Scorecard mapping

Every task ends in one of three states — record it in the task notes:

| Outcome | Meaning | Action |
|---------|---------|--------|
| Blocked + detected | Defense held AND Wazuh fired | Control validated — log in the detection scorecard |
| Blocked + silent | Defense held but no alert | Detection gap — write/adjust a Wazuh rule |
| Found | ARTEX proved a hole | Fix in the next wave, then ARTEX **Retest** to close |
