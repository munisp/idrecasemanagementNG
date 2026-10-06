-- ============================================================================
-- db-optimization.sql — index & table-storage tuning for production load.
-- Apply AFTER all other scripts (init-schemas, casemgmt-core, payments,
-- program-rules, crm-core, onboarding-and-docintel, sharebox).
-- Fully idempotent: safe to re-run on every deploy.
--
-- Design basis: the actual query paths in services/case-api-go and
-- services/outbox-relay-py (see comments per index). The per-tenant loop
-- discovers tenant_* schemas dynamically, so tenants provisioned later are
-- covered by re-running this file; provision_tenant() in init-schemas.sql
-- also stamps the same indexes at birth.
-- ============================================================================

-- Trigram support for ILIKE '%…%' matching:
--   · checks.go memo → invoices.invoice_no contains-match
--   · crm.go account name search
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ============================================================================
-- 1. Per-tenant tables (cases / documents / sealed_offers / outbox)
-- ============================================================================
DO $$
DECLARE sch text;
BEGIN
    FOR sch IN
        SELECT schema_name FROM information_schema.schemata
        WHERE schema_name LIKE 'tenant\_%'
    LOOP
        -- ---- cases ------------------------------------------------------------
        -- Portal list + sweep engines: keyset/offset lists ordered by opened_at,
        -- almost always filtered to actionable (non-closed) cases.
        -- engines.go: WHERE status NOT LIKE 'CLOSED%' AND status<>'SETTLED_IN_NEGOTIATION'
        --             ORDER BY opened_at DESC LIMIT 200
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_open_keyset
            ON %I.cases (opened_at DESC, id)
            WHERE status NOT LIKE ''CLOSED%%'' AND status <> ''SETTLED_IN_NEGOTIATION''', sch);
        -- Full-history keyset pagination fallback (status-filtered lists).
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_opened_id
            ON %I.cases (opened_at DESC, id)', sch);
        -- CRM account 360: cases involving a provider or payer.
        -- crm.go: WHERE provider_id=$1 OR payer_id=$1 ORDER BY opened_at DESC
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_provider
            ON %I.cases (provider_id, opened_at DESC) WHERE provider_id IS NOT NULL', sch);
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_payer
            ON %I.cases (payer_id, opened_at DESC) WHERE payer_id IS NOT NULL', sch);
        -- Batch views.
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_batch
            ON %I.cases (batch_id) WHERE batch_id IS NOT NULL', sch);
        -- Program-specific field filters on details jsonb (casefields.go).
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_details_gin
            ON %I.cases USING gin (details jsonb_path_ops)', sch);
        -- NG phase 6: sector-generic twins — dynamic list filters by subject_line.
        EXECUTE format('CREATE INDEX IF NOT EXISTS cases_subject_line
            ON %I.cases (subject_line, opened_at DESC) WHERE subject_line IS NOT NULL', sch);
        -- HOT updates: updated_at bumps on every touch; leave room so updates
        -- stay on-page and indexes don't bloat.
        EXECUTE format('ALTER TABLE %I.cases SET (fillfactor = 90)', sch);

        -- ---- documents ---------------------------------------------------------
        -- documents.go / casefields.go: WHERE case_id=$1 ORDER BY folder, created_at
        EXECUTE format('CREATE INDEX IF NOT EXISTS documents_case
            ON %I.documents (case_id, created_at DESC)', sch);
        -- sealed_offers: vault reveal + determination joins by case.
        EXECUTE format('CREATE INDEX IF NOT EXISTS sealed_offers_case
            ON %I.sealed_offers (case_id)', sch);

        -- ---- outbox ------------------------------------------------------------
        -- THE hottest query in the platform: the relay polls every tenant schema
        -- continuously. Partial index covers only unpublished rows (tiny).
        -- relay.py: WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED
        EXECUTE format('CREATE INDEX IF NOT EXISTS outbox_unpublished
            ON %I.outbox (id) WHERE published_at IS NULL', sch);
        -- High churn (insert + one update per row, then purge): vacuum early.
        EXECUTE format('ALTER TABLE %I.outbox SET (
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.005)', sch);
    END LOOP;
END $$;

-- ============================================================================
-- 2. Outbox retention — published rows are pure dead weight (they've reached
--    Kafka; bronze keeps the durable copy). Without a purge, outbox tables
--    grow unboundedly and the partial index loses its edge.
--    Schedule via the platform cron (RUNBOOKS.md) — e.g. daily:
--      SELECT public.purge_published_outbox('7 days');
-- ============================================================================
CREATE OR REPLACE FUNCTION public.purge_published_outbox(p_keep interval DEFAULT '7 days')
RETURNS bigint AS $$
DECLARE sch text; total bigint := 0; n bigint;
BEGIN
    FOR sch IN
        SELECT schema_name FROM information_schema.schemata
        WHERE schema_name LIKE 'tenant\_%'
    LOOP
        EXECUTE format('DELETE FROM %I.outbox WHERE published_at IS NOT NULL
                        AND published_at < now() - $1', sch)
            USING p_keep;
        GET DIAGNOSTICS n = ROW_COUNT;
        total := total + n;
    END LOOP;
    RETURN total;
END $$ LANGUAGE plpgsql;

-- ============================================================================
-- 3. Financial tables (payments.sql / program-rules.sql)
-- ============================================================================

-- invoices — receivables, aging, check matching.
-- payments.go:375 GROUP BY status,party · :383 aging WHERE status='OPEN'
-- programops.go listInvoices: tenant (+case/status) ORDER BY created_at DESC
CREATE INDEX IF NOT EXISTS invoices_tenant_status_due
    ON public.invoices (tenant, status, due_date);
CREATE INDEX IF NOT EXISTS invoices_tenant_case
    ON public.invoices (tenant, case_id);
CREATE INDEX IF NOT EXISTS invoices_tenant_created
    ON public.invoices (tenant, created_at DESC);
-- checks.go memo match: invoice_no ILIKE '%'||memo||'%' → trigram GIN.
CREATE INDEX IF NOT EXISTS invoices_invoice_no_trgm
    ON public.invoices USING gin (invoice_no gin_trgm_ops);

-- payments — Stripe/Mojaloop webhook completion looks up by payment_intent
-- (payments.go:317); session_id already UNIQUE.
CREATE INDEX IF NOT EXISTS payments_intent
    ON public.payments (payment_intent) WHERE payment_intent IS NOT NULL;

-- checks — double-scan duplicate lookup (same physical check re-photographed).
-- Non-unique by design: null MICR fields are common pre-OCR.
CREATE INDEX IF NOT EXISTS checks_micr
    ON public.checks (tenant, routing_number, account_number, check_number)
    WHERE routing_number IS NOT NULL;
CREATE INDEX IF NOT EXISTS checks_case ON public.checks (tenant, case_id)
    WHERE case_id IS NOT NULL;

-- Append-only ledgers: never updated → pack pages tight, vacuum rarely.
ALTER TABLE public.financial_events SET (fillfactor = 100);
ALTER TABLE public.payments SET (
    fillfactor = 95,
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_analyze_scale_factor = 0.01);
ALTER TABLE public.checks SET (
    fillfactor = 95,
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_analyze_scale_factor = 0.01);

-- ============================================================================
-- 4. Audit / compliance tables
-- ============================================================================

-- audit_log — verify-audit-chain.py replays per tenant ordered by id.
CREATE INDEX IF NOT EXISTS audit_log_tenant_id ON public.audit_log (tenant, id);
ALTER TABLE public.audit_log SET (fillfactor = 100);

-- sla_breaches / escalations — dashboards + supervisor trail (casemgmt.go:166).
CREATE INDEX IF NOT EXISTS sla_breaches_tenant ON public.sla_breaches (tenant, created_at DESC);
CREATE INDEX IF NOT EXISTS escalations_tenant ON public.escalations (tenant, created_at DESC);
ALTER TABLE public.sla_breaches SET (fillfactor = 100);
ALTER TABLE public.escalations SET (fillfactor = 100);

-- rule_changes / correspondence_log / case_activities / voice_call_logs:
-- append-only streams.
ALTER TABLE public.rule_changes SET (fillfactor = 100);
ALTER TABLE public.correspondence_log SET (fillfactor = 100);
ALTER TABLE public.case_activities SET (fillfactor = 100);
CREATE INDEX IF NOT EXISTS voice_call_logs_tenant ON public.voice_call_logs (tenant, created_at DESC);
CREATE INDEX IF NOT EXISTS voice_intake_tenant ON public.voice_intake_requests (tenant, status, created_at DESC);
ALTER TABLE public.voice_call_logs SET (fillfactor = 100);

-- ============================================================================
-- 5. CRM + work queues
-- ============================================================================

-- accounts list: ORDER BY legal_name with name ILIKE search (crm.go).
CREATE INDEX IF NOT EXISTS accounts_tenant_name ON public.accounts (tenant, legal_name);
CREATE INDEX IF NOT EXISTS accounts_name_trgm ON public.accounts USING gin (legal_name gin_trgm_ops);
ALTER TABLE public.accounts SET (fillfactor = 90);

-- leads list: ORDER BY created_at DESC, id (crm.go:216).
CREATE INDEX IF NOT EXISTS leads_tenant_created ON public.leads (tenant, created_at DESC, id);

-- tasks SLA sweep: WHERE tenant AND status='OPEN' AND due_date IS NOT NULL
-- (casemgmt.go:265). Partial index keeps it tiny.
CREATE INDEX IF NOT EXISTS tasks_open_due ON public.tasks (tenant, due_date)
    WHERE status = 'OPEN' AND due_date IS NOT NULL;
ALTER TABLE public.tasks SET (fillfactor = 90);

-- notifications — bell badge polls unread per user; unread set is tiny.
CREATE INDEX IF NOT EXISTS notifications_unread
    ON public.notifications (tenant, user_sub) WHERE read_at IS NULL;
ALTER TABLE public.notifications SET (
    autovacuum_vacuum_scale_factor = 0.02,
    autovacuum_analyze_scale_factor = 0.01);

-- presence — upsert per active user every 45s: pure churn on a tiny table.
ALTER TABLE public.presence SET (
    fillfactor = 70,
    autovacuum_vacuum_scale_factor = 0.05,
    autovacuum_analyze_scale_factor = 0.02);

-- tasks — completed_at trend query for the ops throughput sparkline.
CREATE INDEX IF NOT EXISTS tasks_done_recent ON public.tasks (tenant, completed_at DESC)
    WHERE status = 'DONE' AND completed_at IS NOT NULL;

-- intake_requests — intake board by status.
CREATE INDEX IF NOT EXISTS intake_tenant_status ON public.intake_requests (tenant, status, created_at DESC);

-- ============================================================================
-- 6. NG sector-agnostic tables (sector-ng.sql / program-manifest.sql)
-- ============================================================================

-- intake_requests.details — dynamic intake fields (policy_number, claim_number,
-- loss_type…) are manifest-defined, so filters land on jsonb paths.
CREATE INDEX IF NOT EXISTS intake_details_gin
    ON public.intake_requests USING gin (details jsonb_path_ops);

-- fee_schedules — effective-dated pricing lookups already carry
-- fee_schedules_lookup (tenant, code, effective_from DESC); pack read-mostly.
ALTER TABLE public.fee_schedules SET (fillfactor = 100);
-- holidays — PK (tenant, day) already covers clock lookups; read-mostly.
ALTER TABLE public.holidays SET (fillfactor = 100);

-- ============================================================================
-- 7. Fresh-tenant parity note
-- ============================================================================
-- provision_tenant() in init-schemas.sql stamps cases/outbox/documents/
-- sealed_offers for NEW tenants; the per-tenant indexes above are stamped
-- there too, so this file is a backfill for pre-existing tenants plus the
-- public-table layer. Keep both in sync when adding indexes.
