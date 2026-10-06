# Go-Live Checklist — Federal IDRE Platform

Sign-off required per row before production cutover. Owner = role, not person.

## 1. Security (blockers)

| # | Item | Verify by | Owner | Sign-off |
|---|------|-----------|-------|----------|
| 1.1 | All secrets from K8s secret store; zero dev credentials in prod manifests | `kubectl get deploy -o yaml \| grep -i idre-secret` returns nothing | Platform | ☐ |
| 1.2 | GitHub PAT rotated; gitleaks clean on full history | CI job `gitleaks` green | Security | ☐ |
| 1.3 | TLS east-west: Kafka 9093, Temporal frontend, OpenSearch, Dapr mTLS | `openssl s_client` each endpoint | Platform | ☐ |
| 1.4 | Keycloak realm review: no demo users, MFA on admin, realm keys rotated | Realm export diff vs `deploy/keycloak/realm-idre.json` | Security | ☐ |
| 1.5 | Permify schema re-applied; rules-admin guard tested negative (auditor write → 403) | e2e authz test against deployed stack | Security | ☐ |
| 1.6 | open-appsec WAF agent deployed and attached to APISIX | Agent pod Running + test payload blocked | Security | ☐ |
| 1.7 | APISIX rate limits live on all routes (incl. `/rules*`, webhooks) | k6 burst → 429s observed | Platform | ☐ |

## 2. Data plane bootstrap (order matters)

| # | Item | Command / note | Owner | Sign-off |
|---|------|----------------|-------|----------|
| 2.1 | Fresh prod cluster (no ZK→KRaft migration; no dev-cluster carryover) | new cluster, helmfile sync | Platform | ☐ |
| 2.2 | Temporal `numHistoryShards: 4096` at **first** bootstrap (immutable) | `tctl admin cluster describe` | Platform | ☐ |
| 2.3 | PgBouncer deployed **before** `max_connections=500` goes live | pgbouncer pool metrics present | Data | ☐ |
| 2.4 | TigerBeetle 6 replicas formatted once; cluster ID matches `TIGERBEETLE_CLUSTER_ID` | `tigerbeetle format` log retained; case-api boots clean | Data | ☐ |
| 2.5 | Apply SQL: init-schemas, payments (incl. `ledger_reconciliation`), program-rules, casemgmt, crm, onboarding, sharebox | psql run log | Data | ☐ |
| 2.6 | Dapr `idre-config` + `idre-resiliency` applied; app Deployments reference them | `kubectl get configuration,resiliency -n idre-core` | Platform | ☐ |
| 2.7 | Kafka topics + ACLs per tenant via `provision-tenant.sh`; `idre.alerts` created | provision log | Platform | ☐ |
| 2.8 | KEDA installed; outbox ScaledObject query regenerated for real schemas (scripts/gen-keda-outbox-query.sh) | `kubectl get scaledobject` Healthy | Platform | ☐ |

## 3. Operability

| # | Item | Verify by | Owner | Sign-off |
|---|------|-----------|-------|----------|
| 3.1 | PrometheusRule alerts live: ledger drift, sla_breaches, Kafka lag, Temporal backlog, PgBouncer saturation | `deploy/kubernetes/prometheus-rules.yaml` applied; test fire each | SRE | ☐ |
| 3.2 | Audit hash-chain verifier runs nightly (CronJob) and pages on mismatch | first scheduled run green | SRE | ☐ |
| 3.3 | PITR restore drill completed (scripts/restore-drill.sh) | drill report attached | Data | ☐ |
| 3.4 | k6 load test at 10× expected peak; capacity numbers in PERFORMANCE.md validated or corrected | k6 report | SRE | ☐ |
| 3.5 | Lakehouse full cycle tested: seed Kafka → bronze → silver → gold CMS report for one tenant | job logs + row counts | Data | ☐ |
| 3.6 | Runbooks rehearsed: ledger drift, outbox backlog, Temporal storm (docs/RUNBOOKS.md) | tabletop sign-off | SRE | ☐ |
| 3.7 | Tenant provisioning dry-run (one non-prod state, incl. teardown) | `provision-tenant.sh --dry-run xx` + live run | Platform | ☐ |

## 4. Compliance

| # | Item | Verify by | Owner | Sign-off |
|---|------|-----------|-------|----------|
| 4.1 | 6-year retention: OpenSearch ISM + Delta VACUUM policy + Postgres PITR 30d window reviewed | policy outputs | Compliance | ☐ |
| 4.2 | Vault master-key rotation procedure documented and rehearsed | rotation drill | Security | ☐ |
| 4.3 | DR RPO/RTO written: Postgres minutes, TB zero-loss ≤2 nodes, lakehouse rebuild ≤ Kafka retention | DR doc approved | Compliance | ☐ |
| 4.4 | Mojaloop provider stays `enabled: false` until a state mandate; staging drill with mock adapter before enabling | config check | Platform | ☐ |

## Known residual risks (accepted explicitly)

- Ray scoring is heuristic (median/MAD) — never present to a regulator as ML.
- Lakehouse hardened but not yet load-tested (see 3.5 — gate on it).
- Ledger reconciliation is daily; intraday drift window is ≤24h by design.
