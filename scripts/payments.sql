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
