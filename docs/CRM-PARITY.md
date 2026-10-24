# CRM Parity — Twenty CRM feature comparison & Salesforce competitive positioning

**Verdict up front:** the platform is now a genuine CRM-based platform. The CRM
object model (accounts, contacts, leads, tasks, notes, pipeline, timeline,
search) is implemented natively in the Go control plane with tenant-isolated
storage — and the regulatory core (statutory workflows, escrow ledger,
double-blind vault) is something neither Twenty nor Salesforce Health Cloud
ships out of the box.

## 1. Twenty CRM — feature-by-feature

| Twenty feature | This platform | Status |
|---|---|---|
| Companies | `accounts` (provider/payer/IDRE/auditor) + 360° view (contacts, disputes, notes) | ✅ implemented |
| People | `contacts` with roles, linked Keycloak users | ✅ implemented |
| Opportunities + kanban pipeline | `cases` + Pipeline view (Initiated → Offer window → Revealed → Determined → Closed) | ✅ implemented |
| Leads | `leads` with source tracking; **voice intake auto-creates leads**; one-click convert → account + contact | ✅ implemented |
| Tasks | `tasks` with assignee, due dates, case linkage, My-tasks queue | ✅ implemented |
| Notes | `notes` on case/account/lead; notes on cases also land in the activity timeline | ✅ implemented |
| Activity timeline | `case_activities`: voice calls (auto-attached), outbound triggers, notes, milestones | ✅ implemented |
| Search | Global search across cases/accounts/contacts/leads (+ OpenSearch full-text on documents) | ✅ implemented |
| Custom fields | `custom` JSONB on accounts/contacts (Salesforce-style extensibility) | ✅ implemented |
| API & webhooks | REST (OpenAPI-shaped) + HMAC voice webhooks + Kafka event streams | ✅ implemented |
| Permissions | Keycloak roles + fail-closed tenant isolation (RLS + schema-per-tenant) | ✅ stronger |
| Workspaces | 50 state tenants with cryptographic + database + ledger isolation | ✅ stronger |
| Workflow automation | Temporal durable workflows (statutory timers, sagas, reminders) | ✅ far stronger |
| Email sync / calendar | Not built — roadmap (notifier service is the seam) | ⏳ roadmap |
| Mobile | PWA (installable, offline) + Capacitor native iOS/Android | ✅ Twenty is web-only |
| AI | Docling + PaddleOCR + VLM document intelligence, Ray ML scoring | ✅ Twenty has none |

## 2. Against the competition (Salesforce)

| Salesforce pillar | Our answer |
|---|---|
| Objects/records + Flow | CRM objects + Temporal workflows — **deterministic, auditable, statute-aware** (Flows can't express "30 business days on a federal calendar, surviving restarts") |
| Omni-Channel | Voice (inbound tools + signed events + outbound triggers, auto-attached to records), email via notifier seam, web intake |
| Reports & dashboards | OpenSearch dashboards + Spark gold-zone CMS marts + DataFusion ad-hoc SQL for auditors |
| Einstein AI | Ray ML (QPA-outlier, settlement propensity) + VLM document extraction with case-record validation |
| Platform encryption | Rust vault: AES-256-GCM envelope encryption, per-tenant HKDF keys — **DBA-proof sealed offers** (Shield doesn't do double-blind) |
| AppExchange/extensibility | Kafka topics + Dapr building blocks + webhook/API-key integrations |
| Compliance | 6-year retention (ISМ + Delta), hash-chained audit trail, CMS monthly reporting built-in |

**Differentiation pitch:** Salesforce gives you a generic CRM you must bend to
45 CFR Part 149; this platform *is* 45 CFR Part 149 with a CRM wrapped around it.

## 3. Honest roadmap (not yet built)

- Email channel (sync + send) and calendar sync
- Drag-and-drop report builder (today: OpenSearch dashboards + SQL)
- Saved views/list views per user (today: filters via API params)
- Kanban drag-to-change-status (view is read-only; status changes stay workflow-gated by design)

## Unified timeline integration (post case-management audit)

Every subsystem writes to ONE activity stream (`public.case_activities`) and one
notification hub (`public.notifications`), so nothing lives in a silo:

| Source | Timeline entry | Notification |
|---|---|---|
| Case initiation (case-api) | `CASE_INITIATED` (case #, service line, QPA, workflow id) | — |
| Workflow signals (case-api) | `RESPONSE_FILED` / `OFFER_SUBMITTED` / `FEES_PAID` … with payload | — |
| Document upload (case-api) | `DOCUMENT_UPLOADED` (filename, size, sealed flag) | — |
| Doc-intel pipeline (Python) | `ANALYSIS_COMPLETE` (doc type, seal/stamp, findings) | — |
| Voice platform | call transcripts, `INTAKE` auto-link | — |
| Email gateway | `EMAIL` (to case or lead) | — |
| Notes on a case | `NOTE` | — |
| Onboarding decision | — | broadcast `ONBOARDING_APPROVE/REJECT` |
| Assignment / escalation / breach | activity + task | targeted + `*` broadcast |

No external vendor dependencies: all services are Go/Rust/Python, all endpoints are
self-hosted (Keycloak, vLLM, MinIO, Temporal, Kafka, Postgres). The only
external calls are NPPES (public registry, with cache fallback) and the
tenant-configured voice platform webhook.
