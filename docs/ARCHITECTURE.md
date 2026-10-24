# NSA Federal IDRE Case Management Platform — Reference Architecture

**Scope:** End-to-end Independent Dispute Resolution (IDR) case management under the No Surprises Act (45 CFR Part 149, incl. the 2026 Final Rule), operated as **50 isolated state tenants** on one platform. Fully self-hosted, **no vendor-portal or hosted-AI dependencies**. English-only UI/API surface.

**Implementation languages:** **Go** (control-plane & transactional APIs), **Rust** (cryptographic vault & ledger-integrity services), **Python** (Temporal workflow workers, lakehouse/ML analytics).

---

## 1. Design principles

1. **Tenant isolation is fail-closed.** Every request carries a tenant (state) context resolved at the gateway; every database row, ledger account, event, workflow, and file object is tenant-scoped. Postgres **schema-per-tenant** for OLTP; TigerBeetle **ledger-per-tenant** for money; Kafka topic prefixing + ACLs for events; Keycloak groups/roles for identity.
2. **Statutes are code, not comments.** Every deadline (30bd open negotiation, 4bd initiation, 3bd response, 10bd offer window, 30bd determination, 30cd payment, suspensions, fee schedules) is a durable Temporal timer + a declarative rule table — never an implicit cron.
3. **Double-blind by cryptography, not by policy.** Sealed offers are AES-256-GCM envelope-encrypted by the Rust vault with per-tenant data keys; the database never sees plaintext; reveal is a lawful-transition protocol, not a query flag.
4. **Money moves only by double-entry.** Escrow, admin fees, IDRE fees, refunds, and settlements are TigerBeetle transfers with balanced postings and idempotency keys. No ad-hoc `UPDATE balance` anywhere.
5. **Everything that happens is an event, and every event is auditable.** Transactional outbox → Kafka → OpenSearch (search/audit) and → Lakehouse (Parquet/Delta) for the 6-year retention and CMS monthly reporting mandates.
6. **Best-of-breed OSS, industry-standard patterns:** hexagonal services, outbox/inbox, saga via Temporal, CQRS read models, GitOps (Argo CD), policy-as-code (OPA), zero-trust east-west (mTLS via Dapr), defense-in-depth (WAF + SIEM + threat intel).

---

## 2. Component map — every requested technology and its role

| # | Component | Role in the platform |
|---|-----------|----------------------|
| 1 | **Kubernetes** | Single substrate for all workloads; namespaces `idre-core`, `idre-data`, `idre-security`, `idre-observability`; HPA, PDBs, NetworkPolicies per namespace. |
| 2 | **APISIX** | North-south API gateway: OIDC token validation (Keycloak JWT), tenant resolution, rate limiting, request validation, mTLS termination, canary routing. |
| 3 | **open-appsec** | ML-based WAF attached to APISIX ingress (appsec agent), blocking OWASP/API abuses before services. |
| 4 | **Keycloak** | Identity: OIDC/OAuth2, MFA, SSO for party portals + admin console; per-state groups (`tenant-tx`, …), role mappers, service accounts for voice/AI integrations. |
| 5 | **Go case-api** | Transactional control plane (cases, parties, offers metadata, fees, documents metadata). Chi/Connect-RPC, pgx to Postgres, Temporal client, Dapr sidecar for pub/sub. |
| 6 | **Rust vault** | Double-blind offer sealing: AES-256-GCM envelope encryption, HKDF per-tenant data keys, sealed-reveal state machine, document encryption at rest. |
| 7 | **TigerBeetle** | Financial ledger: escrow trust accounts, admin-fee remittance, IDRE compensation, refunds — all as double-entry transfers with idempotency. |
| 8 | **Mojaloop** | Payment-settlement interoperability reference: its transfer/ledger/quoting patterns and (optionally) its Payment Manager for real-money rails to party bank accounts; settlement scheme rules modeled on Mojaloop's hub design. |
| 9 | **Temporal** | Durable statutory workflow engine: one long-running workflow per dispute with timers, signals, compensation (sagas); sweeps for SLA breaches. |
| 10 | **Python workflows** | Temporal worker (workflow definitions + activities) — `idre-workflows-py`. |
| 11 | **Kafka** | Backbone event bus: `idre.<tenant>.cases|offers|fees|audit` topics; source for lakehouse bronze, OpenSearch indexing, notifications. |
| 12 | **Fluvio** | Lightweight edge/stream ingestion: per-state SSL (specified-state-law) registry feeds, IDRE directory sync, voice-platform webhooks; SmartModules for schema validation before Kafka bridging. |
| 13 | **Dapr** | Sidecar building blocks: pub/sub abstraction over Kafka, state (Redis), bindings (cron, SMTP/S3), secrets (K8s), service-to-service mTLS + resiliency policies. |
| 14 | **Redis** | Cache (IDRE directory, fee schedules, state configs), distributed locks, rate-limit counters, Temporal frontend not used — cache only, no source of truth. |
| 15 | **Postgres** | OLTP system of record; **schema-per-tenant** + shared `public` reference schema; row-level security as defense-in-depth; logical replication slots for outbox/CDC. |
| 16 | **OpenSearch** | Case full-text search, audit-log analytics (hash-chained events), Wazuh alert backend, dashboarding; ISM policies for 6-year retention to cold/UltraWarm. |
| 17 | **Delta Lake + Parquet** | Lakehouse table format (bronze/silver/gold) on object storage (S3/MinIO); ACID merges for case/fee/audit history. |
| 18 | **Apache Flink** | Streaming ETL: Kafka → bronze Parquet; streaming SLA-breach detection; exactly-once via checkpointing + two-phase commit sink. |
| 19 | **Apache Spark** | Batch/ETL: bronze→silver→gold, CMS monthly report generation, fee aggregations, Delta merge/OPTIMIZE/Z-ORDER. |
| 20 | **Apache DataFusion** | Embeddable SQL engine for ad-hoc gold-zone queries inside a lightweight Go/Python query service (no Spark cluster spin-up). |
| 21 | **Ray** | ML/AI: QPA-outlier scoring, settlement-propensity models, document classification; Ray Serve for inference endpoints behind APISIX. |
| 22 | **Apache Sedona** | Geospatial analytics: provider/plan location vs. state boundaries, air-ambulance corridor analysis, coverage-gap mapping per tenant. |
| 23 | **Wazuh** | SIEM/XDR: agents on nodes, FIM on vault/ledger hosts, vulnerability detection, alerts → OpenSearch, correlation with OpenCTI IOCs. |
| 24 | **OpenCTI** | Threat-intelligence platform: IOC feeds, TTP knowledge; exported indicators feed Wazuh detection rules and APISIX blocklists. |
| 25 | **Kubecost** | FinOps: cost allocation per namespace/label → per-state-tenant chargeback; ledger-compute and lakehouse spend visibility. |

**Supporting (not requested, but required for a complete, dependency-free platform):** MinIO/S3 (object storage for documents + Delta tables), Prometheus + Grafana + Loki (metrics/dashboards/logs), Argo CD (GitOps), cert-manager + Let's Encrypt, OPA/Gatekeeper (policy), Argo Rollouts (progressive delivery), Mailpit/SMTP + any voice-AI webhook (getline.ai-style) adapter.

---

## 3. Architecture at a glance

```
                          ┌────────────────────────── North-South ──────────────────────────┐
 Parties / CMS / IDREs    │  open-appsec WAF  →  APISIX Gateway (OIDC↔Keycloak, tenant ctx) │
 Voice AI (webhooks)       └───────┬───────────────────────────────────┬────────────────────┘
                                   │ mTLS (Dapr)                       │
                 ┌─────────────────▼──────────────┐      ┌─────────────▼──────────────┐
                 │  case-api  (Go)                │      │  vault       (Rust)        │
                 │  cases/offers-meta/fees/docs   │      │  seal/reveal offers, docs  │
                 └──┬───────┬───────┬─────────┬───┘      └──┬───────────────┬────────┘
        pgx         │       │Temp.  │Dapr pub │             │ AES-256-GCM   │ TB transfer
                    │       │client │/Kafka   │             │ HKDF/tenant   │ (seal fee)
          ┌─────────▼──┐  ┌─▼────────────┐  ┌─▼────┐        │               │
          │ PostgreSQL │  │  Temporal    │  │Kafka │◄─ Fluvio edge feeds    │
          │ schema/    │  │  server      │  └──┬───┘  (SSL registries,     │
          │ tenant ×50 │  └─▲────────────┘     │      voice webhooks)      │
          └────────────┘    │ workflows        │                           │
                  ┌─────────┴──────┐           │        ┌──────────────────▼───────┐
                  │ idre-workflows │           │        │  TigerBeetle cluster     │
                  │ (Python worker)│           │        │  ledger-per-tenant ×50   │
                  └────────────────┘           │        │  escrow/admin/IDRE fees  │
                                               │        └──────────────────────────┘
        ┌──────────────────────────────────────┼──────────────── East-West / Data ─────────────┐
        │  Flink (Kafka→bronze, breach stream) │  Wazuh agents (FIM/IDS) → OpenSearch           │
        │  Spark (bronze→silver→gold, reports) │  OpenCTI IOCs → Wazuh rules + APISIX blocklist │
        │  Delta Lake/Parquet on MinIO/S3      │  Keycloak (realm idre, per-state groups)       │
        │  DataFusion (ad-hoc SQL service)     │  Kubecost (per-tenant chargeback)              │
        │  Ray Serve (QPA/settlement ML)       │  Prometheus/Grafana/Loki, Argo CD (GitOps)     │
        │  Sedona (geospatial coverage)        │                                                │
        └──────────────────────────────────────┴────────────────────────────────────────────────┘
```

---

## 4. Multi-tenancy model (50 isolated state tenants)

| Layer | Isolation mechanism |
|-------|---------------------|
| Identity (Keycloak) | One realm `idre`; tenant membership as **groups** `/tenant/tx` … `/tenant/wy`; group claim `tenants` in JWT; per-tenant client roles. Platform/federal admins hold cross-tenant roles. |
| Gateway (APISIX) | JWT plugin validates with Keycloak JWKS; `tenant-router` Lua/Go plugin extracts the state code, injects `X-Tenant-Code`, **rejects** tokens whose tenant claim doesn't match the path/body tenant (fail-closed). |
| OLTP (Postgres) | `tenant_tx` … `tenant_wy` schemas, identical DDL; `search_path` pinned per connection by middleware; **RLS policies** on every table keyed by `current_setting('app.tenant')`. Cross-tenant queries are impossible from app roles. |
| Money (TigerBeetle) | One TB **ledger per tenant** (ledger ID = FIPS-ish numeric map). Accounts carry tenant prefix in `user_data_128`. |
| Events (Kafka) | Topic pattern `idre.<state>.<domain>`; ACLs grant producer/consumer per prefix; Flink/Ray jobs route by topic. |
| Workflows (Temporal) | Workflow ID = `IDR-<STATE>-<case>`; task queues partitioned per region; namespace `idre`. |
| Files (MinIO/S3) | Bucket `idre-docs`, key prefix `<state>/cases/<id>/…`; bucket policy denies cross-prefix for service accounts. |
| Lakehouse | Delta tables partitioned by `tenant` column; per-tenant views for auditors. |
| Cost (Kubecost) | Labels `tenant=tx` on tenant-specific pods; shared services amortized by usage-weighted allocation. |

**Specified State Law (SSL) routing:** `state_config` (per tenant, in `public.state_config` + Redis cache) carries SSL scope, citations, ground-ambulance inclusion, and thresholds. Eligibility evaluation routes SSL-covered items to the state process (`PENDING_REVIEW`) and never opens federal timelines for them.

---

## 5. Language allocation & service inventory

| Service | Lang | Why |
|---|---|---|
| `case-api` | **Go** | High-throughput transactional REST/Connect API; pgx; Temporal & Dapr SDKs are first-class in Go; single static binary, tiny containers. |
| `vault` | **Rust** | Cryptographic trust boundary (sealed offers, document envelope encryption); memory safety for key material (`zeroize`); axum + `aes-gcm` + `hkdf`. |
| `idre-workflows` | **Python** | Temporal Python SDK; statutory rule tables and date engines are fastest to maintain in Python; activities call Go/Rust services over mTLS. |
| `analytics` | **Python** | PySpark, PyFlink, DataFusion (bindings), Ray, Sedona — all Python-native. |
| `ledger-adapter` *(optional)* | **Rust** | Thin sidecar exposing TigerBeetle to non-Go/Python clients with idempotent HTTP semantics; Go/Python use official clients directly. |

Repository is a **polyrepo-ready monorepo**: shared protobuf/OpenAPI contracts in `contracts/`, each service independently deployable via its own Dockerfile + Helm chart values.

---

## 6. Statutory workflow engine (Temporal)

One **long-running workflow per dispute**: `IdrCaseWorkflow` (Python). Signals = events (response filed, offer submitted, fees paid, determination issued…). Timers = statutory clocks on a **business-day calendar** (computed federal holidays + per-state extra dates).

| Statutory clock (45 CFR 149 / 2026 Final Rule) | Temporal mechanism |
|---|---|
| 30 business days open negotiation | `workflow.timer(business_days(30))` from `OPEN_NEGOTIATION_START` |
| 4 business days to initiate IDR | branching timer; expiry → `INITIATION_WINDOW_MISSED` |
| 3 business days to respond | timer from initiation notice |
| 10 business days offer window | timer; expiry auto-locks window (`WINDOW_EXPIRED` reveal path) |
| 30 business days to determination | timer from selection finalization; breach → alert + CMS report flag |
| 30 calendar days payment (prevailing party) | timer from determination; breach → penalty flag |
| 30 business days refund of overpayment | timer from overpayment identification |
| 90cd single-item / 30bd batched suspension | suspension registry entries auto-expire |
| Monthly CMS report ≤ 30bd after month-end | cron-scheduled `CmsMonthlyReportWorkflow` per tenant |

**Why Temporal and not cron:** durable timers survive restarts; every signal/timer/activity is in the workflow history = a **secondary audit trail**; sagas give compensation (e.g., reverse fee reservation if eligibility fails); sweeps are child workflows with bounded continue-as-new.

---

## 7. Money movement (TigerBeetle + Mojaloop patterns)

### 7.1 TigerBeetle ledger design (per tenant)

```
Accounts (per tenant ledger)                      Transfers
─────────────────────────────────────────         ───────────────────────────────────────
1000 ESCROW_TRUST_HELD                            T1 fee invoice reservation   pending → post
2000 PARTY_RECEIVABLE_<partyId>                   T2 admin-fee remittance      escrow→admin
3000 ADMIN_FEE_REMITTANCE                         T3 IDRE compensation         escrow→idre
4000 IDRE_COMPENSATION                            T4 refund to losing/winning  escrow→refund
5000 REFUND_PAYABLE                               T5 settlement split          linked transfers
```

- **Double-entry invariant:** every transfer debits+credits within the same ledger; TigerBeetle enforces balance constraints (`debits_must_not_exceed_credits` where required).
- **Idempotency:** transfer `id = hash(case_id, transfer_type, party_id)` — safe retries from workflows.
- **Pending transfers** model invoice-at-initiation (2-phase: reserve → post on selection finalization or void on withdrawal) — exactly the escrow lifecycle.
- **Linked transfers** model atomic multi-leg settlement splits (e.g., tie → 50/50 IDRE fee split).
- TigerBeetle replicas run as a 3-node StatefulSet (see `deploy/helm-values/tigerbeetle.yaml`), data on local NVMe/PVC, IO ring tuned per upstream guidance.

### 7.2 Mojaloop alignment

Mojaloop is a payment-switch reference architecture (transfers, quoting, settlement, DFSP onboarding). We adopt:

- **Transfer lifecycle** (prepare → fulfil → reject) mapped to TigerBeetle pending/post/void — identical 2-phase semantics.
- **Scheme rules & participant onboarding** for IDRE entities: each certified IDRE = a "participant" with settlement account and fee schedule; onboarding records mirror Mojaloop's participant model.
- **Optional Payment Manager / SDK-scheme-adapter** as the boundary if the operator later connects real rails (ACH/ RTP) for party refunds; until then Mojaloop's open-source components serve as the **settlement-reporting schema** (positions, limits, settlement windows) implemented over TigerBeetle balances in the gold zone.

This keeps the platform dependency-free while staying wire-compatible with a proven payment-hub design.

---

## 8. Double-blind offer vault (Rust)

**Threat model:** even a DBA with full Postgres access must not learn an offer amount before lawful reveal.

- **Envelope encryption:** master key in K8s Secret (or external KMS/Vault via Dapr secret store) → HKDF-SHA256 derives **per-tenant data key** → per-offer random **DEK**; ciphertext + wrapped DEK + nonce stored in Postgres as opaque blobs.
- **Sealed state machine:** `SEALED → (BOTH_SUBMITTED | WINDOW_EXPIRED | RULE_DEFAULT) → REVEALED`. Reveal is performed *inside* the vault service, which verifies workflow state with Temporal (describe-workflow call) before decrypting. Tampering with DB flags alone cannot reveal.
- **Documents:** same envelope scheme per object; plaintext streams only through the vault; MinIO stores ciphertext only.
- **Key hygiene:** `zeroize` on drop; keys never logged; optional HSM/KMS envelope root for production.
- Service: `services/vault-rs` (axum, mTLS-only listener, called by Go case-api and Python activities).

---

## 9. Eventing & streaming (Kafka + Fluvio + Dapr)

### 9.1 Kafka topics (per tenant prefix)

| Topic | Producer | Consumers |
|---|---|---|
| `idre.<st>.cases` | case-api (outbox) | Flink→bronze, OpenSearch indexer, notifier |
| `idre.<st>.offers` | vault (metadata only — never amounts pre-reveal) | Flink, audit indexer |
| `idre.<st>.fees` | ledger-adapter/case-api | gold-zone settlement, Kubecost feed |
| `idre.<st>.audit` | all services (hash-chained) | OpenSearch (6-yr ISM), lakehouse immutable bronze |
| `idre.<st>.voice` | voice webhook adapter | intake router, call-log indexer |

**Outbox pattern:** services write business row + outbox row in one Postgres tx; a Debezium/pg-logical publisher (or a small Go poller) ships to Kafka. No dual-write anomalies.

### 9.2 Fluvio edge ingestion

Fluvio runs lightweight **per-integration** connectors where Kafka clients are too heavy:

- **State SSL registry feeds** (23 ground-ambulance states, all-payer MD, etc.): Fluvio inbound connectors poll/webhook state registry APIs, SmartModules validate schema, bridge into `idre.<st>.ssl-registry`.
- **Voice-platform webhooks** (getline.ai-style): Fluvio HTTP source receives `call.completed` events, normalizes `dynamic_variables`, bridges to `idre.<st>.voice`.
- Why Fluvio here: Rust-based, MB-scale footprint, declarative connectors — ideal as the DMZ ingestion tier feeding the Kafka backbone.

### 9.3 Dapr building blocks

| Building block | Backing | Used by |
|---|---|---|
| pub/sub | Kafka component | case-api publish; notifier subscribe |
| state | Redis | IDRE directory cache, rate-limit buckets |
| bindings | cron (report triggers), S3 (document lifecycle) | workflows, doc archiver |
| secrets | K8s secret store | vault master key, API keys |
| service invocation | mTLS sidecar-to-sidecar | case-api→vault, workflows→case-api |
| resiliency | timeouts/retries/circuit breakers | all east-west calls |

---

## 10. Data platform (Lakehouse)

**Medallion zones on MinIO/S3 in Delta/Parquet:**

```
bronze/  raw Kafka archive (append-only, partitioned by tenant/date)     ← Flink streaming sink
silver/  conformed: cases, parties, offers(revealed), fees, deadlines    ← Spark structured batch
gold/    marts: cms_monthly_report, sla_breaches, qpa_benchmarks,
         settlement_positions (Mojaloop-style), geo_coverage             ← Spark + Sedona
ad-hoc   DataFusion query service over gold (sub-second, no cluster)     ← Go/Python service
ml       Ray: QPA-outlier scoring, settlement propensity, doc classify   ← Ray Train/Serve
geo      Sedona: provider-vs-state-boundary, air-ambulance corridors     ← Spark+Sedona
```

- **Flink** (streaming): Kafka→bronze exactly-once (Parquet bulk sink under `bronze-raw/<domain>/tenant=<st>/date=…`, one sink per domain×tenant, checkpoint-committed); **streaming SLA early-warning** is a keyed process function (key = case_id) that persists each case's tracked deadlines in checkpointed state and fires a processing-time timer WARN_AHEAD_DAYS before the deadline — the warning fires even when no new event arrives for the case; it reads each tenant's manifest clocks (5-min refresh) and emits to the `idre.alerts` ops topic before Temporal timers fire. Audit events land in their own append-only `bronze-raw/audit` domain — the immutable 6-year archive.
- **Spark** (batch): incremental, idempotent bronze→silver: every bronze row is keyed by a content fingerprint (sha256 of tenant|event_time|payload) and MERGEd into silver, and `silver/_control` records a per-tenant high-water event_time so each run reads only newer rows (replays dedupe on the fingerprint; `--full` replays history safely). Silver carries typed common columns (etype, case_id, provider_id, payer_id, status, service_line, event_at) + raw payload (drift-proof). Silver→gold marts: `cms_monthly_report` (every metric scoped by EVENT month via `month_bounds` — closures count in the month they closed), `sla_breach_summary`, `qpa_benchmarks` (cells published only at n≥30), `settlement_positions` (latest ledger snapshot per case). Delta `OPTIMIZE` + Z-ORDER on silver; **no `VACUUM` on silver** (6-year retention is a legal mandate; gold is regenerable and may be vacuumed). **CMS monthly report** per tenant (≤30bd mandate).
- **DataFusion**: embedded SQL in a small query service exposing `POST /analytics/query` — Keycloak JWT-authenticated (JWKS-verified), tenant scope from the token (`tenant` claim / `/tenant/<st>` group; X-Tenant-Code override only for platform/federal admins), literal/comment-aware read-only SQL gate with an engine-enforced row cap, and gold marts read through the Delta log with a tenant partition filter. Auditors get instant SQL without Spark.
- **Ray**: ML workloads; trainers read silver/cases through the Delta log, fit deterministic numpy logistic/median-MAD models, and persist parameters to `gold/ml_models/<model>/tenant=<st>/model.json`; Ray Serve endpoints (behind APISIX, JWT-protected) load persisted params with periodic refresh and report `insufficient_data` below the 300-sample training floor — no untrained constants are ever served.
- **Sedona**: spatial joins of provider service locations vs Census/state shapefiles; federal-vs-SSL jurisdiction checks for border-line cases; coverage-gap dashboards. Inputs are platform-produced: `pg_export.py` feeds `silver/providers` from `public.entities`, `load_us_states.py` feeds `ref/us_states` from a mounted Census TIGER GeoJSON (no runtime network dependency); the geo job validates both and skips marts with a logged reason when coordinates aren't captured yet.
- **OpenSearch**: operational search (cases, documents OCR text, audit), Wazuh alert indices, 6-yr retention via ISM (`hot 90d → warm 2y → cold → delete 6y`).

---

## 11. Security architecture

| Layer | Control |
|---|---|
| Edge | open-appsec WAF on APISIX (ML detection, API schema enforcement, rate limiting, bot defense). |
| Identity | Keycloak OIDC, MFA, per-state groups; short-lived tokens; service accounts for integrations (voice, registries) with least-privilege roles. |
| East-west | Dapr mTLS (Sentry CA); NetworkPolicies default-deny; OPA/Gatekeeper pod policies. |
| Data | Postgres RLS; vault envelope encryption; TigerBeetle dedicated network segment; MinIO SSE. |
| Detection | **Wazuh** agents on all nodes (FIM on vault/TB data dirs, log analysis, vuln scans) → OpenSearch; alerts correlated with **OpenCTI** IOCs (STIX/TAXII feeds); matched IOCs pushed as blocklists to APISIX. |
| Audit | Hash-chained append-only audit log (sha256(prevHash‖payload)) written per mutation; anchored daily into lakehouse immutable bronze; `verify-chain` admin endpoint. |
| Secrets | K8s Secrets + optional external KMS; Dapr secret store abstraction; no secrets in env files in the repo (`.env.example` only). |
| Compliance | 6-year retention (OpenSearch ISM + Delta retention), CMS monthly reporting (Temporal cron + Spark gold), HIPAA-adjacent controls (PHI never in logs — Dapr middleware redaction). |

---

## 12. Voice-platform integration (getline.ai-style)

Voice AI platforms of this class expose two integration surfaces, both supported:

1. **Webhook "tools" (agent → platform):** during a call, the voice agent calls
   `POST /api/voice/tools/case-status` / `.../deadlines` / `.../intake` on our APISIX-exposed voice router. Auth = per-tenant API key (Keycloak service account or hashed key in Postgres). The Go `voice-adapter` (part of case-api) answers with concise JSON the agent reads to the caller (case status, next statutory deadline, fee balance).
2. **Event webhooks (platform → us):** `call.completed`/`call.transcript` posted to `POST /api/voice/events` — HMAC-SHA256 signature verified (`X-Signature` header, per-tenant secret in Dapr secret store), `dynamic_variables` mapped to `case_number`, `caller`, `intent`. Events persist to `voice_call_logs` and `voice_intake_requests` and publish to `idre.<st>.voice` via Fluvio→Kafka.

Outbound milestone triggers (offer window closing in 2bd, determination issued) are emitted as Temporal activities → notifier service → voice platform's outbound-call API (if enabled per tenant config).

---

## 13. Deployment topology (Kubernetes)

```
namespaces
├── idre-core        apisix+open-appsec, case-api(Go), vault(Rust), workflows(Python),
│                    keycloak, postgres(CNPG), redis, temporal, tigerbeetle(3), dapr control
├── idre-data        kafka(strimzi), fluvio, minio, flink, spark, ray, opensearch
├── idre-security    wazuh manager+agents, opencti(+elastic/rabbit/redis deps)
├── idre-finops      kubecost
└── idre-observ      prometheus, grafana, loki, tempo(optional)
```

- **GitOps:** Argo CD app-of-apps; each component = Helm chart + values in `deploy/helm-values`; environment overlays via Kustomize.
- **Progressive delivery:** Argo Rollouts canary for case-api/vault.
- **Resilience:** PDBs on postgres/TB/kafka; PodAntiAffinity for TB replicas; HPA on case-api/workflows.
- **Cost:** Kubecost per-tenant labels → monthly chargeback report lands in gold zone next to CMS report.

---

## 14. Local development & production path

- **Local:** `docker compose up` (this repo) — Postgres, Redis, Kafka(KRaft), Temporal+UI, TigerBeetle, Keycloak, APISIX, MinIO, OpenSearch, Flink/Spark (single-node), and the three app services. Wazuh/OpenCTI/Kubecost are k8s-only (documented, not composed) due to footprint.
- **Prod:** managed-by-us Helm/helmfile on any Kubernetes (EKS/GKE/AKS/bare-metal). No vendor portal, no hosted AI services, no external control plane. Object storage = any S3-compatible endpoint.

---

## 15. Repository layout

```
nsa-idre-platform/
├── docs/ARCHITECTURE.md                ← this document
├── docker-compose.yml                  ← full local stack
├── services/
│   ├── case-api-go/                    Go control-plane API (Temporal client, TB ledger, Dapr pub)
│   ├── vault-rs/                       Rust sealed-offer/document vault (AES-256-GCM + HKDF)
│   ├── idre-workflows-py/              Temporal workflows (statutory lifecycle, CMS reports)
│   ├── analytics-py/                   Spark/Flink/DataFusion/Ray/Sedona jobs
│   └── graph-intel-py/                 FalkorDB dispute graph, numpy GraphSAGE link prediction,
│                                       EPR-KGQA over local ollama, lakehouse bridge (see docs/GRAPH-INTELLIGENCE.md)
├── deploy/
│   ├── kubernetes/                     namespaces, tigerbeetle statefulset, rbac
│   ├── helm-values/                    one values file per component
│   ├── keycloak/realm-idre.json        realm, clients, roles, tenant groups
│   ├── apisix/apisix.yaml              standalone routes + OIDC + WAF hook
│   ├── dapr/components/                pubsub.kafka, state.redis, bindings, secrets
│   └── opensearch/                     ISM policies, index templates
└── scripts/                            dev helpers (seed, verify-chain, smoke)
```


---

## 16. Document management & intelligent analysis (doc-intel)

**Storage core:** uploads stream through `case-api` → encrypted by `vault` (AES-256-GCM, per-tenant data key, AAD-bound to the object key) → ciphertext-only objects in MinIO (`idre-docs/<state>/cases/<id>/<doc>.enc`) → metadata + version row in Postgres → `doc.uploaded` event on Kafka. Downloads reverse the path with authorization, sealed-document guards (offer justifications stay locked until lawful reveal), and audit logging. Retention is 6 years (federal mandate) enforced by WORM bucket policy + OpenSearch/Delta retention — aligned with the audit-log ISM policy.

**Intelligent analysis** is a dedicated Python service (`services/doc-intel-py`) consuming `idre.*.documents` events:

```
doc.uploaded → fetch ciphertext → vault decrypt → composable pipeline:
  classify → Docling (PDF/DOCX/HTML parsing: layout, reading order, tables,
  formulas, its own OCR) → [PaddleOCR, conditionally docked only for
  low-text-coverage scans] → PP-StructureV3 seal/stamp detection
  → VLM extraction (PaddleOCR-VL or Qwen2.5-VL via any OpenAI-compatible
  endpoint) → cross-validation vs. case record
→ Postgres (structured result + findings) → OpenSearch (full-text + extracted fields)
→ Kafka doc.analyzed → Temporal signal DOC_ANALYZED
```

- **Docling-first, composable pipeline:** stages are plugins declared in `pipeline.yaml` (order, enable/disable, per-stage config, and `when:` conditions). **IBM Docling** is the primary parser (structure-aware markdown, tables as dataframes, layout regions); PaddleOCR is docked in only when Docling reports low text coverage (scanned documents); PP-StructureV3 adds seal/stamp detection that Docling lacks. Swap the VLM with one env var (`VLM_ENDPOINT`/`VLM_MODEL`); add a stage by implementing `stage_<name>()` and appending a YAML entry. Extraction schemas are declared per doc type (`idr_claim`, `eob`, `determination_letter`).
- **IDR-specific extraction:** claim numbers, CPT/HCPCS codes, billed vs. QPA amounts, provider TIN/NPI, service dates, denial codes, determination outcomes, signature/seal presence.
- **Validation:** extracted QPA is cross-checked against the case record; mismatches surface as findings (`ANALYZED_WITH_FINDINGS`) rather than silently passing.
- **Double-blind preserved:** sealed offer documents are marked `SEALED_PENDING_REVEAL` and never analyzed before lawful reveal.
- **Failure handling:** poison messages are parked (`ERROR` status), commits are manual (at-least-once), Temporal signals are best-effort since analysis is already durable.

## 17. Stakeholder onboarding (per tenant/state)

Onboarding is a **durable Temporal workflow**, not a script — every stakeholder type has application → verification → approval → provisioning → activation with full audit:

| Stakeholder | Gates | Approval authority | Provisioning on approval |
|---|---|---|---|
| **IDRE entity** | CMS certification check (directory), EIN, fee-schedule band validation, COI attestation, state-specific extras | FEDERAL_ADMIN only | Keycloak account (ARBITRATOR role, tenant group), TigerBeetle settlement accounts, portal invite |
| **Provider org** | EIN + NPPES NPI registry lookup, W-9 via doc-intel | CASE_MANAGER (tenant) or FEDERAL_ADMIN | Keycloak PARTY account + tenant group |
| **Payer org** | EIN, NAIC code, W-9 | same | same |
| **State auditor org** | state credential letter | same | STATE_AUDITOR role (read-only views) |
| **Admin staff** | internal sponsorship | CASE_MANAGER/FEDERAL_ADMIN | CASE_MANAGER role |

Key mechanics:

- **Per-state requirements matrix** (`state_config.onboarding_requirements`, JSONB): states with SSL programs can demand extra artifacts (e.g., state arbitration certification); the workflow checks the matrix and rejects with an explicit `missing` list.
- **Document verification is the doc-intel pipeline**: onboarding documents (W-9, certification, COI) are analyzed by the same PaddleOCR+VLM pipeline; the workflow blocks on the `DOCS_VERIFIED` signal with a 10-day SLA and 3-day reminder cadence.
- **Approval authority matrix is enforced twice**: in the gateway API (role check) and inside the workflow (durable record of approver, decision, reason).
- **Auto-provisioning** on approval: Keycloak user with correct role + `/tenant/<state>` group, TigerBeetle settlement accounts for IDREs (Mojaloop participant model), invite email — all as compensated, retryable activities.
- **Rejection/expiry paths** are first-class statuses (`REJECTED_AUTO`, `EXPIRED`, `REJECTED`) with reasons, not silent drops.
- **Tenant-level onboarding** (`TenantOnboardingWorkflow`) durably brings a whole state live: schema → ledger → topics → Keycloak group → config → smoke checks, auditable step by step.


---

## 18. Frontend: PWA + native mobile (`portal/`)

One framework-free codebase covers every backend surface (coverage matrix in
`docs/STAKEHOLDERS.md` §3):

- **PWA**: manifest + service worker — installable on iOS/Android/desktop; app shell
  cached offline; API GETs are network-first with cached fallback and an explicit
  offline indicator; mutations never fire offline. Served by nginx in compose
  (proxies `/v1` to APISIX); in k8s it deploys as a static Deployment behind the gateway.
- **Native mobile**: Capacitor wraps the same web assets into iOS/Android shells
  (`capacitor.config.json`, `cap sync`, store builds via Xcode/Android Studio).
  Push notifications register with the same notifier service Temporal activities use.
- **Auth**: Keycloak OIDC + PKCE (S256) implemented without libraries; silent refresh;
  roles and tenant groups drive navigation and per-screen actions.
- **Role-aware screens**: parties get sealed-offer submission and fee actions;
  arbitrators get the determination form; case managers get onboarding approvals and
  the voice console; federal admins/auditors get compliance reports and SLA breaches.

Also closed in this pass: **NPPES offline fallback** — `check_ein_npi` uses live NPPES
with a 5s timeout, falls back to the `public.npi_cache` (refreshed on every live hit,
seedable from the NPPES weekly bulk file), and records `NPI_VERIFICATION_DEGRADED`
in the audit trail instead of blocking onboarding during registry outages.
