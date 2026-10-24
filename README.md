# NSA Federal IDRE Case Management Platform

Self-hosted, multi-tenant (50 isolated state tenants) case-management platform for CMS
No Surprises Act federal Independent Dispute Resolution — **fully self-hosted,
no vendor-portal dependencies**. English-only surface.

**Stack:** Go · Rust · Python | Kubernetes · APISIX + open-appsec · Keycloak · PostgreSQL ·
Redis · Temporal · TigerBeetle (+ Mojaloop settlement patterns) · Kafka · Fluvio · Dapr ·
OpenSearch · MinIO/S3 lakehouse (Delta Lake + Parquet) · Flink · Spark · DataFusion · Ray ·
Sedona · Wazuh · OpenCTI · Kubecost.

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — full reference architecture: component map,
  tenancy model, statutory workflow engine, ledger design, double-blind vault, data platform,
  security, voice-AI integration, deployment topology.

## Services

| Service | Language | Path |
|---|---|---|
| Case control-plane API (JWT auth, tenant gateway, Temporal/TigerBeetle clients, documents, onboarding, voice webhooks) | Go | `services/case-api-go` |
| Double-blind offer/document vault (AES-256-GCM + HKDF per-tenant keys) | Rust | `services/vault-rs` |
| Temporal statutory lifecycle + stakeholder onboarding workflows + CMS report cron | Python | `services/idre-workflows-py` |
| Document intelligence: IBM Docling (primary parser) + PaddleOCR fallback + PP-StructureV3 seals + VLM extraction (composable pipeline) | Python | `services/doc-intel-py` |
| Lakehouse jobs (Spark/Delta, Flink, DataFusion, Ray, Sedona) | Python | `services/analytics-py` |

## Toolchain persistence (survives sandbox/server restarts)

Go 1.23.4 + Rust 1.98.1 and their dependency caches live in `/mnt/agents/toolchains`
(the only persistent storage; large files are stored as reassembled tar chunks).
After any restart — **no reinstall, no network**:

```bash
bash scripts/restore-toolchains.sh   # extracts toolchains + caches, runs offline build smoke tests
source scripts/env.sh                # PATH/GOPATH/CARGO_HOME + registry mirrors
```

Verified: restore into a clean home, then `GOPROXY=off go build` and
`CARGO_NET_OFFLINE=true cargo check` both pass offline.

## Quick start (local)

```bash
docker compose up -d                       # full stack, one command
./scripts/provision-tenant.sh tx 148       # tenant provisioning (also bootstrapped in init SQL)

# Get a Keycloak token (dev realm, see deploy/keycloak/realm-idre.json)
TOKEN=$(curl -s http://localhost:8085/realms/idre/protocol/openid-connect/token \
  -d grant_type=password -d client_id=case-portal \
  -d username=cm.tx -d password=password123 | jq -r .access_token)

# Initiate a dispute in the Texas tenant
curl -X POST http://localhost:8080/v1/tenants/tx/cases/initiate \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"case_number":"CMS-TX-2026-00001","service_line":"ER","plan_type":"SELF_FUNDED",
       "qpa_cents":180000,"provider_id":"p1","payer_id":"i1","open_negotiation_end":"2026-10-30"}'

# Temporal UI: http://localhost:8233 — watch the statutory timers run.
```

## Kubernetes (production)

```bash
kubectl apply -f deploy/kubernetes/               # namespaces, TigerBeetle, network policies
helmfile -f deploy/helmfile.yaml sync             # every component, one fleet install
kubectl apply -f deploy/dapr/components/          # Dapr building blocks
# Keycloak realm auto-imports via keycloakConfigCli; APISIX routes in deploy/apisix.
```

## Security & compliance highlights

- Fail-closed tenant isolation at identity, gateway, database (schema + RLS), ledger, topic, and object-key layers.
- Sealed offers are cryptographically inaccessible to DBAs until lawful reveal (vault verifies workflow state).
- All money movement is TigerBeetle double-entry with idempotency keys; escrow uses pending→post/void transfers.
- Hash-chained append-only audit log, 6-year retention (OpenSearch ISM + Delta retention), CMS monthly reporting workflow.
- Wazuh SIEM + OpenCTI IOC correlation + open-appsec WAF; mTLS east-west via Dapr.
- Voice-AI integration (getline.ai-style): API-key tool endpoints + HMAC-signed event webhooks; outbound milestone triggers from Temporal activities.

## Billing note

This platform is fully self-hosted infrastructure you operate; costs are your own cloud/hardware
spend. Kubecost provides per-state-tenant chargeback reporting.
