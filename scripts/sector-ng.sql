-- NG phases 2–4 DDL — sector-agnostic data structures.
-- Apply after program-manifest.sql.

-- Phase 2: sector-specific intake fields collected by the dynamic form land here.
ALTER TABLE public.intake_requests
    ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT '{}';

-- Phase 3: effective-dated fee schedules (WC fee matrices, FL Estimated Cost
-- Matrix, appraisal rate cards). Historical cases price against the rates in
-- force on their anchor date — annual adjustments never rewrite the past.
CREATE TABLE IF NOT EXISTS public.fee_schedules (
    id              bigserial PRIMARY KEY,
    tenant          text NOT NULL,
    code            text NOT NULL,              -- CPT/HCPCS, task code, tariff line
    label           text,
    amount_cents    bigint NOT NULL,
    effective_from  date NOT NULL,
    effective_to    date,                       -- null = current
    UNIQUE (tenant, code, effective_from)
);
CREATE INDEX IF NOT EXISTS fee_schedules_lookup
    ON public.fee_schedules (tenant, code, effective_from DESC);

-- Phase 4: jurisdiction holidays for business-day clock counting.
-- Federal holidays seeded per tenant; programs add their own (e.g. FL state
-- holidays) via the admin API. Clock counting reads this table.
CREATE TABLE IF NOT EXISTS public.holidays (
    tenant text NOT NULL,
    day    date NOT NULL,
    name   text NOT NULL,
    PRIMARY KEY (tenant, day)
);

-- US federal holidays 2026 (Observed) for the seed tenants.
INSERT INTO public.holidays (tenant, day, name)
SELECT t.tenant, d.day, d.name FROM (VALUES ('fl'), ('tx')) AS t(tenant)
CROSS JOIN (VALUES
    ('2026-01-01'::date, 'New Year''s Day'),
    ('2026-01-19', 'Martin Luther King Jr. Day'),
    ('2026-02-16', 'Washington''s Birthday'),
    ('2026-05-25', 'Memorial Day'),
    ('2026-06-19', 'Juneteenth'),
    ('2026-07-03', 'Independence Day (observed)'),
    ('2026-09-07', 'Labor Day'),
    ('2026-10-12', 'Columbus Day'),
    ('2026-11-11', 'Veterans Day'),
    ('2026-11-26', 'Thanksgiving Day'),
    ('2026-12-25', 'Christmas Day')
) AS d(day, name)
ON CONFLICT DO NOTHING;

-- FL Estimated Cost Matrix (illustrative seed — replace with AHCA's published
-- matrix; rows are effective-dated so yearly adjustments are inserts, not edits).
INSERT INTO public.fee_schedules (tenant, code, label, amount_cents, effective_from)
VALUES
    ('fl', 'REVIEW_SIMPLE',  'Claims dispute review — simple',   35000, '2026-01-01'),
    ('fl', 'REVIEW_MEDICAL', 'Claims dispute review — medical review required', 57500, '2026-01-01'),
    ('fl', 'FILING',         'Filing fee',                        25000, '2026-01-01')
ON CONFLICT DO NOTHING;

-- Phase 6: generic evidence fields on existing tenant case tables
-- (new tenants get these from init-schemas.sql; this backfills old ones).
DO $$
DECLARE t text;
BEGIN
    FOR t IN SELECT schema_name FROM information_schema.schemata
             WHERE schema_name LIKE 'tenant\_%' LOOP
        EXECUTE format('ALTER TABLE %I.cases
            ADD COLUMN IF NOT EXISTS subject_line text,
            ADD COLUMN IF NOT EXISTS benchmark_cents bigint', t);
        EXECUTE format('UPDATE %I.cases SET
            subject_line   = coalesce(subject_line, service_line),
            benchmark_cents = coalesce(benchmark_cents, qpa_cents)', t);
    END LOOP;
END $$;
