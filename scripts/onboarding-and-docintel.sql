-- Author: Patrick Munis at newwave
-- Additions: document intelligence + stakeholder onboarding + doc versioning.

ALTER TABLE public.state_config
    ADD COLUMN IF NOT EXISTS onboarding_requirements jsonb NOT NULL DEFAULT '{}';
-- Shape: { "IDRE_ENTITY": ["cms_certification_number","fee_schedule","coi_attestation","w9"],
--          "PROVIDER_ORG": ["npi","w9"], "PAYER_ORG": ["naic_code","w9"], ... }

-- documents.version: provision_tenant() stamps it for new tenants; backfill existing ones:
DO $$
DECLARE st text;
BEGIN
    FOREACH st IN ARRAY ARRAY['al','ak','az','ar','ca','co','ct','de','fl','ga','hi','id','il','in','ia','ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv','nh','nj','nm','ny','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','tx','ut','vt','va','wa','wv','wi','wy'] LOOP
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS version int NOT NULL DEFAULT 1', 'tenant_'||st);
    END LOOP;
END $$;

CREATE TABLE IF NOT EXISTS public.doc_analysis (
    doc_id      uuid PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     uuid,
    status      text NOT NULL DEFAULT 'QUEUED',  -- QUEUED|SEALED_PENDING_REVEAL|ANALYZED|ANALYZED_WITH_FINDINGS|ERROR
    doc_type    text,                            -- idr_claim | eob | determination_letter | ...
    result      jsonb NOT NULL DEFAULT '{}',     -- extracted fields, findings, seal detection, tables
    analyzed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS doc_analysis_tenant_case ON public.doc_analysis (tenant, case_id);

-- PENDING_DOCS had no way to ever clear: StakeholderOnboardingWorkflow waits
-- on a DOCS_VERIFIED signal fed by doc-intel, but there was no document-
-- upload endpoint for an application (only /cases/{caseId}/documents did),
-- so every application requiring docs sat in PENDING_DOCS until the 10-day
-- SLA silently expired it. application_id is nullable for the same reason
-- case_id already is: one doc_analysis table serves both document kinds,
-- never both at once. The FK to stakeholder_applications is added after
-- that table below (forward reference).
ALTER TABLE public.doc_analysis ADD COLUMN IF NOT EXISTS application_id uuid;
CREATE INDEX IF NOT EXISTS doc_analysis_tenant_application ON public.doc_analysis (tenant, application_id);

CREATE TABLE IF NOT EXISTS public.stakeholder_applications (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL REFERENCES public.state_config(tenant),
    type         text NOT NULL,   -- IDRE_ENTITY|PROVIDER_ORG|PAYER_ORG|STATE_AUDITOR_ORG|ADMIN_STAFF
    legal_name   text NOT NULL,
    ein_masked   text,            -- full EIN stored encrypted via vault; masked copy here
    npi          text,
    payload      jsonb NOT NULL DEFAULT '{}',  -- type-specific: fee schedule, COI, banking (vault-sealed)...
    status       text NOT NULL DEFAULT 'SUBMITTED',
    status_reason text DEFAULT '',
    submitted_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS apps_tenant_status ON public.stakeholder_applications (tenant, status);

-- ADD CONSTRAINT has no IF NOT EXISTS form in Postgres (unlike ADD COLUMN) --
-- this script is run repeatedly, so a plain ADD CONSTRAINT would fail on
-- every run after the first.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'doc_analysis_application_id_fkey') THEN
        ALTER TABLE public.doc_analysis
            ADD CONSTRAINT doc_analysis_application_id_fkey
            FOREIGN KEY (application_id) REFERENCES public.stakeholder_applications(id);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS public.application_documents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    application_id uuid NOT NULL REFERENCES public.stakeholder_applications(id),
    object_key    text NOT NULL,
    size_bytes    int,
    content_type  text,
    uploaded_by   text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS application_documents_app ON public.application_documents (tenant, application_id);

CREATE TABLE IF NOT EXISTS public.idre_directory (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    legal_name  text NOT NULL,
    cms_cert_number text NOT NULL UNIQUE,
    certified   boolean NOT NULL DEFAULT true,
    fee_single_low int, fee_single_high int, fee_batched int,  -- 2026 bands: 425-800 / 1125
    states_served text[] NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- NPPES offline fallback cache (seedable from the weekly NPPES bulk file;
-- refreshed on every successful live lookup).
CREATE TABLE IF NOT EXISTS public.npi_cache (
    npi        text PRIMARY KEY,
    valid      boolean NOT NULL,
    source     text NOT NULL DEFAULT 'live',   -- live | bulk_seed
    checked_at timestamptz NOT NULL DEFAULT now()
);

-- CRM activity timeline (case record feed: voice calls, notes, milestones).
CREATE TABLE IF NOT EXISTS public.case_activities (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    type       text NOT NULL,   -- VOICE_CALL | NOTE | MILESTONE | OUTBOUND_TRIGGER
    body       text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS case_activities_case ON public.case_activities (tenant, case_id);

-- Voice platform outbound configuration per tenant.
ALTER TABLE public.voice_configs
    ADD COLUMN IF NOT EXISTS platform_base_url text,     -- e.g. https://api.getline.ai
    ADD COLUMN IF NOT EXISTS outbound_api_key  text;     -- vault-sealed in prod
