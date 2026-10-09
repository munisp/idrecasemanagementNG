-- Case-management layer: relationships, batching, checklists, notifications,
-- saved views, assignment state. (Applied per tenant where noted.)

-- Assignment state on cases (per-tenant tables) — applied to every provisioned
-- tenant schema. provision_tenant() creates cases without these columns, so this
-- DO loop is the single source of truth for the assignment/batching columns.
DO $$
DECLARE st text;
BEGIN
    FOREACH st IN ARRAY ARRAY['al','ak','az','ar','ca','co','ct','de','fl','ga','hi','id','il','in','ia','ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv','nh','nj','nm','ny','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','tx','ut','vt','va','wa','wv','wi','wy'] LOOP
        EXECUTE format('ALTER TABLE %I.cases
            ADD COLUMN IF NOT EXISTS assigned_to text,
            ADD COLUMN IF NOT EXISTS assigned_role text,
            ADD COLUMN IF NOT EXISTS batch_id uuid,
            ADD COLUMN IF NOT EXISTS parent_case_id uuid,
            ADD COLUMN IF NOT EXISTS duplicate_of uuid,
            ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT ''{}''::jsonb', 'tenant_'||st);
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_assignee ON %I.cases (assigned_to) WHERE assigned_to IS NOT NULL', 'tenant_'||st);
    END LOOP;
END $$;

-- Related cases: batch groups, parent/child, duplicates.
CREATE TABLE IF NOT EXISTS public.case_relationships (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     text NOT NULL,
    related_case_id text NOT NULL,
    rel_type    text NOT NULL,      -- BATCH | PARENT_CHILD | DUPLICATE
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, related_case_id, rel_type)
);

-- Configurable stage checklists (e.g. 12-element determination checklist).
CREATE TABLE IF NOT EXISTS public.case_checklists (
    id        bigserial PRIMARY KEY,
    tenant    text NOT NULL,
    case_id   text NOT NULL,
    stage     text NOT NULL,        -- INTAKE | ELIGIBILITY | OFFERS | DETERMINATION | PAYMENT
    item      text NOT NULL,
    required  boolean NOT NULL DEFAULT true,
    done      boolean NOT NULL DEFAULT false,
    done_by   text,
    done_at   timestamptz,
    UNIQUE (tenant, case_id, stage, item)
);

-- In-app notifications (bell): assignments, escalations, milestones, mentions.
CREATE TABLE IF NOT EXISTS public.notifications (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    user_sub   text NOT NULL,       -- keycloak sub; '*' = role broadcast marker
    type       text NOT NULL,       -- ASSIGNMENT | ESCALATION | MILESTONE | MENTION | SLA_BREACH
    body       text NOT NULL,
    link       text,                -- portal hash, e.g. #/cases/<id>
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notifications_user ON public.notifications (tenant, user_sub, read_at);

-- Per-user saved views (Salesforce "list views").
CREATE TABLE IF NOT EXISTS public.saved_views (
    id        bigserial PRIMARY KEY,
    tenant    text NOT NULL,
    user_sub  text NOT NULL,
    object    text NOT NULL,        -- CASES | ACCOUNTS | LEADS | TASKS
    name      text NOT NULL,
    filters   jsonb NOT NULL DEFAULT '{}',
    pinned    boolean NOT NULL DEFAULT false,   -- pinned views surface first in L2 nav
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, user_sub, object, name)
);
-- Reproduced live: on a database where public.saved_views already existed
-- (an earlier deploy of a schema version before `pinned` was added), the
-- CREATE TABLE above is a no-op, so listSavedViews' SELECT ... pinned ...
-- failed with "column pinned does not exist" -- silently, since the
-- handler discards the query error (`out, _ :=`) and serializes a nil
-- slice as JSON null, which the portal then crashes on (`saved.find is
-- not a function` / "Cannot read properties of null").
ALTER TABLE public.saved_views ADD COLUMN IF NOT EXISTS pinned boolean NOT NULL DEFAULT false;

-- Escalation log (SLA breach -> supervisor trail).
CREATE TABLE IF NOT EXISTS public.escalations (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    clock      text NOT NULL,
    level      int NOT NULL DEFAULT 1,
    escalated_to text,              -- role or sub
    detail     text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Live presence: portal pings POST /presence/ping every 45s while active;
-- "online" = last_seen within 3 minutes. Postgres (not Redis) so presence
-- survives cache flushes and joins the ops dashboard in one query.
CREATE TABLE IF NOT EXISTS public.presence (
    tenant       text NOT NULL,
    user_sub     text NOT NULL,
    display_name text,
    roles        jsonb NOT NULL DEFAULT '[]',
    last_seen    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, user_sub)
);
CREATE INDEX IF NOT EXISTS presence_seen ON public.presence (tenant, last_seen DESC);

-- User preferences: theme, density, last tenant, palette recents — the
-- portal keeps a device-local copy for offline/instant paint, but this table
-- is the source of truth so prefs follow the user across PWA/desktop/native.
CREATE TABLE IF NOT EXISTS public.user_prefs (
    tenant     text NOT NULL,
    user_sub   text NOT NULL,
    key        text NOT NULL,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, user_sub, key)
);


-- Copilot action batches (Phase 3): a bounded, human-approved batch of
-- agentic actions proposed by the local model from platform-verified facts.
-- The Temporal CopilotActionBatchWorkflow gates execution on a staff
-- APPROVE signal; every action is allowlisted and its result recorded here.
CREATE TABLE IF NOT EXISTS public.copilot_action_batches (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      uuid NOT NULL,
    status       text NOT NULL DEFAULT 'PENDING_APPROVAL', -- PENDING_APPROVAL|APPROVED|REJECTED|EXPIRED|APPLIED|PARTIAL|FAILED
    proposed_by  text NOT NULL,          -- staff subject who requested the proposal
    model        text NOT NULL,          -- local model that produced the proposal
    actions      jsonb NOT NULL,         -- [{type, params, result?, error?}] — allowlisted, ≤5
    rationale    text,                   -- model's one-paragraph justification (advisory)
    decided_by   text,
    decided_at   timestamptz,
    workflow_id  text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS copilot_batches_case ON public.copilot_action_batches (tenant, case_id, created_at DESC);
CREATE INDEX IF NOT EXISTS copilot_batches_pending ON public.copilot_action_batches (tenant, status) WHERE status = 'PENDING_APPROVAL';

-- Copilot conversational threads (Assistant surface): both directions of
-- every turn, per case, with the model attribution — the thread is part of
-- the case record.
CREATE TABLE IF NOT EXISTS public.copilot_threads (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    uuid NOT NULL,
    role       text NOT NULL,          -- user | assistant
    body       text NOT NULL,
    model      text,                   -- assistant turns only
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS copilot_threads_case ON public.copilot_threads (tenant, case_id, created_at);


-- Party voice (conversation-first step 4): counterparty chat turns on secure
-- share links. token_fp is a sha256 prefix of the share token — correlation
-- for the record without ever storing the bearer credential itself.
CREATE TABLE IF NOT EXISTS public.party_threads (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    uuid NOT NULL,
    token_fp   text NOT NULL,
    role       text NOT NULL,          -- party | assistant
    body       text NOT NULL,
    model      text,                   -- assistant turns only; NULL = fallback
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS party_threads_token ON public.party_threads (token_fp, created_at);
CREATE INDEX IF NOT EXISTS party_threads_case ON public.party_threads (tenant, case_id, created_at);

-- Bulk dispute intake (third-party filers: RCM vendors, legal reps, plan
-- delegates). One row per submitted batch; results jsonb holds the per-row
-- outcomes so a retried submission replays instead of double-filing.
CREATE TABLE IF NOT EXISTS public.intake_batches (
    id            bigserial PRIMARY KEY,
    tenant        text        NOT NULL,
    submitter     text        NOT NULL,
    batch_ref     text        NOT NULL,
    item_count    int         NOT NULL,
    created_count int         NOT NULL DEFAULT 0,
    error_count   int         NOT NULL DEFAULT 0,
    results       jsonb       NOT NULL DEFAULT '[]',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, submitter, batch_ref)
);
CREATE INDEX IF NOT EXISTS intake_batches_tenant_created ON public.intake_batches (tenant, created_at DESC);

-- Time entries (per-role effort on each dispute): append-only through the
-- API; the record itself is the audit trail. Role is the principal's real
-- role at entry time. Weekly per-dispute and monthly team rollups are
-- computed by /reports/time. Billable links back to eligibility: only
-- eligible cases' time should be billed.
CREATE TABLE IF NOT EXISTS public.time_entries (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     uuid NOT NULL,
    subject     text NOT NULL,
    role        text NOT NULL,
    entry_date  date NOT NULL,
    minutes     int  NOT NULL CHECK (minutes > 0 AND minutes <= 1440),
    note        text NOT NULL DEFAULT '',
    billable    boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS time_entries_tenant_case ON public.time_entries (tenant, case_id);
CREATE INDEX IF NOT EXISTS time_entries_tenant_date ON public.time_entries (tenant, entry_date);

-- Billable rates per role per tenant: managed by FEDERAL_ADMIN/
-- PLATFORM_ADMIN only. Reports multiply minutes by the rate of the role the
-- entry was logged under; entries whose role has no rate report hours with
-- no amount (never a guessed rate).
CREATE TABLE IF NOT EXISTS public.time_rates (
    tenant              text NOT NULL,
    role                text NOT NULL,
    rate_cents_per_hour int  NOT NULL CHECK (rate_cents_per_hour >= 0),
    updated_by          text NOT NULL,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, role)
);

-- ============================================================
-- Service-fee invoicing engine (billing.go) + reconciliation
-- engine with accounting adapters (recon.go).
-- ============================================================

-- Invoices generated from the time ledger (time_entries x time_rates) over a
-- parameterized period. Rates are snapshotted onto lines at generation time;
-- invoices are immutable once ISSUED (void, never edit).
CREATE TABLE IF NOT EXISTS public.billing_invoices (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant         text NOT NULL,
    invoice_no     text NOT NULL,
    period_start   date NOT NULL,
    period_end     date NOT NULL,              -- exclusive, same convention as /reports/time
    case_id        text,                       -- NULL = consolidated period invoice
    bill_to_name   text NOT NULL,
    bill_to_email  text,
    status         text NOT NULL DEFAULT 'DRAFT', -- DRAFT|APPROVED|ISSUED|PAID|VOID
    subtotal_cents bigint NOT NULL,
    tax_cents      bigint NOT NULL DEFAULT 0,
    total_cents    bigint NOT NULL,
    currency       text NOT NULL DEFAULT 'usd',
    due_date       date,
    memo           text,
    created_by     text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, invoice_no)
);
-- Idempotency: at most one ACTIVE (non-VOID) invoice per period + case scope.
CREATE UNIQUE INDEX IF NOT EXISTS billing_invoices_period_uniq
    ON public.billing_invoices (tenant, period_start, period_end, coalesce(case_id, ''))
    WHERE status <> 'VOID';
CREATE INDEX IF NOT EXISTS billing_invoices_tenant ON public.billing_invoices (tenant, status, created_at DESC);

CREATE TABLE IF NOT EXISTS public.billing_invoice_lines (
    id                  bigserial PRIMARY KEY,
    invoice_id          uuid NOT NULL REFERENCES public.billing_invoices(id),
    tenant              text NOT NULL,
    case_id             text NOT NULL,
    case_number         text,
    role                text NOT NULL,
    minutes             bigint NOT NULL,
    rate_cents_per_hour bigint NOT NULL,       -- snapshot at generation time
    amount_cents        bigint NOT NULL,
    description         text NOT NULL
);
CREATE INDEX IF NOT EXISTS billing_lines_invoice ON public.billing_invoice_lines (invoice_id);

-- Payments recorded against issued invoices (FINANCE). ref carries the
-- remittance/bank reference the reconciliation engine keys on.
CREATE TABLE IF NOT EXISTS public.billing_invoice_payments (
    id           bigserial PRIMARY KEY,
    invoice_id   uuid NOT NULL REFERENCES public.billing_invoices(id),
    tenant       text NOT NULL,
    amount_cents bigint NOT NULL CHECK (amount_cents > 0),
    method       text NOT NULL,                -- ach|wire|check|card
    ref          text,
    received_at  date NOT NULL,
    recorded_by  text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS billing_payments_invoice ON public.billing_invoice_payments (invoice_id);

-- Per-invoice event trail (generation, transitions, payments).
CREATE TABLE IF NOT EXISTS public.billing_invoice_events (
    id         bigserial PRIMARY KEY,
    invoice_id uuid NOT NULL REFERENCES public.billing_invoices(id),
    tenant     text NOT NULL,
    event      text NOT NULL,                  -- GENERATED|APPROVED|ISSUED|VOIDED|PAYMENT_RECORDED
    actor      text NOT NULL,
    detail     jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS billing_events_invoice ON public.billing_invoice_events (invoice_id);

-- Per-tenant, per-year invoice number sequence (atomic allocation).
CREATE TABLE IF NOT EXISTS public.billing_sequences (
    tenant text NOT NULL,
    year   int  NOT NULL,
    next   bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (tenant, year)
);

-- Reconciliation: one batch per external feed import (accounting platform
-- export or bank statement), items are the external transactions.
CREATE TABLE IF NOT EXISTS public.recon_batches (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    source       text NOT NULL,                -- csv:quickbooks | csv:xero | http:<feed> | ...
    period_start date NOT NULL,
    period_end   date NOT NULL,                -- exclusive
    status       text NOT NULL DEFAULT 'IMPORTED', -- IMPORTED|MATCHED|CLOSED
    total_items  int  NOT NULL DEFAULT 0,
    matched      int  NOT NULL DEFAULT 0,
    unmatched    int  NOT NULL DEFAULT 0,
    exceptions   int  NOT NULL DEFAULT 0,
    imported_by  text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS recon_batches_tenant ON public.recon_batches (tenant, id DESC);

CREATE TABLE IF NOT EXISTS public.recon_items (
    id               bigserial PRIMARY KEY,
    batch_id         bigint NOT NULL REFERENCES public.recon_batches(id),
    tenant           text NOT NULL,
    txn_date         date NOT NULL,
    amount_cents     bigint NOT NULL,          -- signed: + received, - disbursed
    reference        text,
    description      text,
    status           text NOT NULL DEFAULT 'UNMATCHED', -- UNMATCHED|MATCHED|EXCEPTION|IGNORED
    matched_event_id bigint REFERENCES public.financial_events(id),
    match_kind       text,                     -- exact_ref|tolerance|manual
    matched_by       text,
    matched_at       timestamptz,
    note             text
);
CREATE INDEX IF NOT EXISTS recon_items_batch ON public.recon_items (batch_id, status);
CREATE INDEX IF NOT EXISTS recon_items_tenant_date ON public.recon_items (tenant, txn_date);

-- Accounts payable subledger (arap.go). Obligations are explicit records:
-- created by the determination-award hook (syncAwardPayable) or by FINANCE,
-- settled with method + remittance ref (which recon matches to bank feeds).
CREATE TABLE IF NOT EXISTS public.payables (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    case_id       text,
    payee         text NOT NULL,
    source        text NOT NULL DEFAULT 'other',  -- award|refund|vendor|tax|other
    amount_cents  bigint NOT NULL CHECK (amount_cents > 0),
    due_date      date,
    status        text NOT NULL DEFAULT 'OPEN',   -- OPEN|SETTLED|VOID
    settled_at    date,
    settle_method text,
    settle_ref    text,                           -- recon keys on this
    note          text,
    created_by    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- One OPEN award payable per case (upserted by the award hook).
CREATE UNIQUE INDEX IF NOT EXISTS payables_award_uniq
    ON public.payables (tenant, case_id) WHERE source = 'award' AND status = 'OPEN';
CREATE INDEX IF NOT EXISTS payables_tenant ON public.payables (tenant, status, created_at DESC);

-- Payment -> dispute allocation: every dollar recorded against a billing
-- invoice is split across its case lines (explicit or proportional split),
-- so receivables and collections roll up per dispute.
CREATE TABLE IF NOT EXISTS public.billing_payment_allocations (
    id          bigserial PRIMARY KEY,
    payment_id  bigint NOT NULL REFERENCES public.billing_invoice_payments(id),
    invoice_id  uuid   NOT NULL REFERENCES public.billing_invoices(id),
    tenant      text   NOT NULL,
    case_id     text   NOT NULL,
    amount_cents bigint NOT NULL CHECK (amount_cents >= 0)
);
CREATE INDEX IF NOT EXISTS billing_alloc_case ON public.billing_payment_allocations (tenant, case_id);
CREATE INDEX IF NOT EXISTS billing_alloc_payment ON public.billing_payment_allocations (payment_id);

-- ─────────────────────────────────────────────────────────────────────
-- Third-party administrators (TPA): organizations that file/track disputes
-- on behalf of one or more initiating parties. Self-serve onboarding via a
-- one-time claim code; states retain suspend as after-the-fact control.
-- No audit_log in NG — these rows carry their own timestamps; the record
-- is the audit trail.
-- ─────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS public.tpas (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant        text NOT NULL,
  name          text NOT NULL,
  contact_name  text NOT NULL DEFAULT '',
  contact_email text NOT NULL,
  claim_code    text,                    -- one-time; burned on claim
  status        text NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE | SUSPENDED
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, contact_email)
);
CREATE UNIQUE INDEX IF NOT EXISTS tpas_claim_code_uniq ON public.tpas (tenant, claim_code) WHERE claim_code IS NOT NULL;

CREATE TABLE IF NOT EXISTS public.tpa_users (
  tenant    text NOT NULL,
  user_sub  text NOT NULL,
  tpa_id    uuid NOT NULL REFERENCES public.tpas(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, user_sub)
);

-- Initiating parties a TPA acts for. party_type uses manifest codes
-- (PartyACode/PartyBCode) — never hard-coded healthcare labels.
CREATE TABLE IF NOT EXISTS public.tpa_clients (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant        text NOT NULL,
  tpa_id        uuid NOT NULL REFERENCES public.tpas(id),
  party_name    text NOT NULL,
  party_type    text NOT NULL,
  contact_email text NOT NULL DEFAULT '',
  status        text NOT NULL DEFAULT 'ACTIVE',
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, tpa_id, party_name)
);

-- Origin of every TPA-filed dispute: who it BELONGS to (initiating party)
-- vs who filed/acts (the TPA). Keyed by intake_id (staff-driven intake);
-- case_id attaches on conversion (advanceIntake hook).
CREATE TABLE IF NOT EXISTS public.case_origin (
  tenant                 text NOT NULL,
  intake_id              uuid NOT NULL,
  tpa_id                 uuid NOT NULL REFERENCES public.tpas(id),
  client_id              uuid NOT NULL REFERENCES public.tpa_clients(id),
  initiating_party_name  text NOT NULL,
  initiating_party_type  text NOT NULL,
  filed_by_email         text NOT NULL,
  case_id                uuid,
  created_at             timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant, intake_id)
);
CREATE INDEX IF NOT EXISTS case_origin_tpa_idx ON public.case_origin (tenant, tpa_id);
CREATE INDEX IF NOT EXISTS case_origin_case_idx ON public.case_origin (tenant, case_id) WHERE case_id IS NOT NULL;

-- ─────────────────────────────────────────────────────────────────────
-- Bank rails (bank.go): payables gain ACH destinations + batch linkage;
-- NACHA payout batches persist their generated file for re-download.
-- ─────────────────────────────────────────────────────────────────────
ALTER TABLE public.payables ADD COLUMN IF NOT EXISTS destination jsonb;
ALTER TABLE public.payables ADD COLUMN IF NOT EXISTS payout_batch_id uuid;
CREATE TABLE IF NOT EXISTS public.bank_payout_batches (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant         text NOT NULL,
  file_reference text NOT NULL,
  entry_count    int  NOT NULL,
  total_cents    bigint NOT NULL,
  status         text NOT NULL DEFAULT 'GENERATED',
  created_by     text NOT NULL DEFAULT '',
  file_body      text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant, file_reference)
);
