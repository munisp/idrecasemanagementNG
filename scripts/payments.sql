-- Author: Patrick Munis at newwave
-- Card payment collection (Stripe Checkout) on top of the invoices ledger.
-- Every checkout session, payment intent, and refund is persisted here so the
-- financial dashboard reconciles Stripe payouts against receivables without
-- calling the Stripe API at report time.

CREATE TABLE IF NOT EXISTS public.payments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant         text NOT NULL,
    case_id        text NOT NULL,
    invoice_id     uuid NOT NULL REFERENCES public.invoices(id),
    provider       text NOT NULL DEFAULT 'stripe',
    session_id     text UNIQUE,              -- Stripe Checkout Session cs_…
    payment_intent text,                     -- pi_… (set on completion)
    amount_cents   bigint NOT NULL,
    currency       text NOT NULL DEFAULT 'usd',
    payer_email    text,
    payer_name     text,                     -- auto-filled from checkout (Field Criteria: who paid)
    payer_org      text,
    status         text NOT NULL DEFAULT 'PENDING', -- PENDING|PAID|FAILED|EXPIRED|REFUNDED
    stripe_fee_cents bigint,                 -- from balance transaction (when available)
    raw            jsonb NOT NULL DEFAULT '{}',     -- last webhook payload (audit)
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payments_tenant ON public.payments (tenant, status, created_at DESC);
CREATE INDEX IF NOT EXISTS payments_case ON public.payments (tenant, case_id);

-- Unified financial event stream for the dashboard: one row per money
-- movement or obligation, regardless of source (invoice lifecycle, card
-- payment, fee transfer, refund).
CREATE TABLE IF NOT EXISTS public.financial_events (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    case_id      text,
    invoice_id   uuid,
    kind         text NOT NULL,   -- INVOICE_ISSUED|PAYMENT_INITIATED|PAYMENT_PAID|PAYMENT_FAILED|
                                  -- REFUND_ISSUED|INVOICE_VOIDED|FEE_TRANSFER
    direction    text NOT NULL,   -- IN (receivable collected) | OUT (refund/payout) | NONE
    amount_cents bigint NOT NULL,
    party        text,            -- HEALTH_PLAN|PROVIDER|IDRE|ESCROW
    ref          text,            -- stripe pi_…, remittance ref, tigerbeetle transfer id
    actor        text,            -- user sub or 'stripe-webhook'
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS fin_events_tenant ON public.financial_events (tenant, created_at DESC);
CREATE INDEX IF NOT EXISTS fin_events_case ON public.financial_events (tenant, case_id);

-- Postgres <-> TigerBeetle reconciliation results (Temporal daily cron per
-- tenant: LedgerReconciliationWorkflow -> reconcile_ledger activity).
CREATE TABLE IF NOT EXISTS public.ledger_reconciliation (
    id             bigserial PRIMARY KEY,
    tenant         text NOT NULL,
    check_name     text NOT NULL,   -- clearing_check | ledger_invariant
    expected_cents bigint NOT NULL,
    actual_cents   bigint NOT NULL,
    drift_cents    bigint NOT NULL,
    status         text NOT NULL,   -- OK | DRIFT
    ran_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_recon_tenant ON public.ledger_reconciliation (tenant, ran_at DESC);
-- Physical check intake: photo/scan -> OCR/ICR extraction -> invoice match ->
-- clearing confirmation -> settlement. Images are vault-sealed in MinIO
-- (checks/<id>.bin); this table holds metadata + extraction + match state.
CREATE TABLE IF NOT EXISTS public.checks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant          text NOT NULL,
    status          text NOT NULL DEFAULT 'RECEIVED',
        -- RECEIVED -> PROCESSED -> MATCHED | REVIEW -> CLEARED | REJECTED | RETURNED
    object_key      text NOT NULL,          -- MinIO key of the sealed image
    content_type    text,
    -- extracted fields (doc-intel check_processor)
    routing_number  text,
    account_number  text,
    check_number    text,
    amount_cents    bigint,                 -- courtesy amount (numeric box)
    legal_amount_cents bigint,              -- written line (ICR; may differ)
    amount_mismatch boolean NOT NULL DEFAULT false,
    check_date      date,
    payer_name      text,
    memo            text,                   -- often carries the invoice number
    confidence      text NOT NULL DEFAULT 'low',   -- high|medium|low
    extraction      jsonb NOT NULL DEFAULT '{}',   -- raw OCR/ICR detail (audit)
    -- match + settlement
    invoice_id      uuid REFERENCES public.invoices(id),
    case_id         text,
    payment_id      uuid,
    matched_by      text,                   -- memo_invoice_no|amount|manual
    cleared_at      timestamptz,            -- funds confirmed (bank/lockbox)
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS checks_tenant ON public.checks (tenant, status, created_at DESC);
CREATE INDEX IF NOT EXISTS checks_invoice ON public.checks (invoice_id);
-- ---------------------------------------------------------------------------
-- Payment rails (moov-io stack): vault-sealed bank accounts, generated payment
-- files (ACH debits, Fedwire, X9.37 image cash letters, Metro 2 furnishing),
-- and OFAC sanctions screening results.
-- ---------------------------------------------------------------------------

-- Bank details for parties/arbitrators/TPAs/vendors. The account number is
-- NEVER stored in plaintext: vault /docs/seal encrypts under the tenant data
-- key (AAD = object key), the DB holds only the sealed blob + last4.
CREATE TABLE IF NOT EXISTS public.payment_accounts (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant                text NOT NULL,
    owner_type            text NOT NULL,          -- PARTY|ARBITRATOR|TPA|VENDOR
    owner_ref             text NOT NULL,          -- party name / user sub / org code
    label                 text NOT NULL DEFAULT '',
    routing_number        text NOT NULL,          -- 9-digit ABA (validated, fed-lookup)
    bank_name             text NOT NULL DEFAULT '',-- from FedACH directory when available
    account_sealed        bytea NOT NULL,         -- vault-sealed account number
    account_last4         text NOT NULL,
    account_type          text NOT NULL DEFAULT 'CHECKING', -- CHECKING|SAVINGS
    ach_debit_authorized  boolean NOT NULL DEFAULT false,   -- NACHA authorization on file
    created_by            text NOT NULL DEFAULT '',
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_accounts_tenant ON public.payment_accounts (tenant, owner_type, owner_ref);

-- Every payment file the platform generates or ingests, one row per file.
CREATE TABLE IF NOT EXISTS public.payment_files (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    kind         text NOT NULL,   -- ACH_DEBIT|ACH_RETURN|WIRE|ICL|METRO2
    reference    text NOT NULL,   -- human-facing file reference
    item_count   int  NOT NULL DEFAULT 0,
    total_cents  bigint NOT NULL DEFAULT 0,
    status       text NOT NULL DEFAULT 'GENERATED', -- GENERATED|SENT|ACKED|RETURNED|FAILED
    file_body    text NOT NULL DEFAULT '',          -- outbound file contents
    detail       jsonb NOT NULL DEFAULT '{}',
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, kind, reference)
);
CREATE INDEX IF NOT EXISTS payment_files_tenant ON public.payment_files (tenant, kind, created_at DESC);

-- Per-item tracing for generated files (and the item-level target of ACH
-- returns: a return file carries original trace numbers back to these rows).
CREATE TABLE IF NOT EXISTS public.payment_file_items (
    id            bigserial PRIMARY KEY,
    tenant        text NOT NULL,
    file_id       uuid NOT NULL REFERENCES public.payment_files(id),
    invoice_id    uuid,
    payable_id    uuid,
    check_id      uuid,
    amount_cents  bigint NOT NULL DEFAULT 0,
    trace_number  text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'PENDING',  -- PENDING|SENT|RETURNED|SETTLED
    return_code   text,
    return_reason text,
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_file_items_trace ON public.payment_file_items (tenant, trace_number);
CREATE INDEX IF NOT EXISTS payment_file_items_file ON public.payment_file_items (file_id);

-- OFAC sanctions screening record (watchman-backed). Append-only: every
-- screening decision is preserved for examiner review.
CREATE TABLE IF NOT EXISTS public.screening_checks (
    id                bigserial PRIMARY KEY,
    tenant            text NOT NULL,
    subject_type      text NOT NULL,     -- ARBITRATOR|TPA|PARTY|VENDOR|MANUAL
    subject_ref       text NOT NULL DEFAULT '',
    query_name        text NOT NULL,
    matched           boolean NOT NULL,
    top_score         numeric(5,4) NOT NULL DEFAULT 0,
    matches           jsonb NOT NULL DEFAULT '[]',
    list_entity_count int NOT NULL DEFAULT 0,
    list_refreshed_at timestamptz,
    screened_by       text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS screening_checks_tenant ON public.screening_checks (tenant, subject_type, created_at DESC);


-- Proof-of-authorization artifact for ACH WEB debits (NACHA 2.5.1 / WEB
-- authorization retention): the signed authorization (PDF/scan/text) is
-- vault-sealed like the account number itself; only metadata is queryable.
ALTER TABLE public.payment_accounts
    ADD COLUMN IF NOT EXISTS ach_authorization_sealed bytea,
    ADD COLUMN IF NOT EXISTS ach_authorization_name  text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ach_authorization_at    timestamptz;

-- NACHA reinitiation bookkeeping (case-api enforces the rules):
-- attempt_count tracks originations per invoice (cap: original + 2 for
-- R01/R09; other return codes may not be reinitiated at all). Item status
-- gains RETRY_ELIGIBLE (R01/R09 under cap) and SETTLED (return window passed).
ALTER TABLE public.payment_file_items
    ADD COLUMN IF NOT EXISTS attempt_count int NOT NULL DEFAULT 1;
