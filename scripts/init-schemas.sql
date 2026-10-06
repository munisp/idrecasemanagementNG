-- Bootstrap DDL for the NSA IDRE platform.
-- Schema-per-tenant: identical DDL stamped into tenant_<state> for all 50 states.
-- Run by Postgres container init (docker-compose) or by provision-tenant.sh (k8s).

CREATE TABLE IF NOT EXISTS public.state_config (
    tenant             text PRIMARY KEY,          -- two-letter state code
    tb_ledger_id       int  NOT NULL,             -- TigerBeetle ledger id
    ssl_program        boolean NOT NULL DEFAULT false,  -- specified-state-law program
    ssl_citation       text,                      -- e.g. 'Tex. Ins. Code ch. 1271'
    ssl_scope          text,                      -- fully-insured lines covered
    ground_ambulance   boolean NOT NULL DEFAULT false,
    all_payer_model    boolean NOT NULL DEFAULT false,  -- MD
    extra_holidays     jsonb NOT NULL DEFAULT '[]'
);

-- Shared reference + integration tables (cross-tenant by design)
CREATE TABLE IF NOT EXISTS public.voice_api_keys (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL REFERENCES public.state_config(tenant),
    label      text NOT NULL,
    key_hash   text NOT NULL UNIQUE,              -- sha256 of the API key
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.voice_configs (
    tenant         text PRIMARY KEY REFERENCES public.state_config(tenant),
    webhook_secret text NOT NULL,                 -- HMAC secret for inbound events
    outbound_enabled boolean NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS public.voice_call_logs (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    direction   text NOT NULL,     -- INBOUND_TOOL | INBOUND_EVENT | OUTBOUND_TRIGGER
    tool        text,
    caller_phone text,
    case_number text,
    summary     text,
    status      text NOT NULL DEFAULT 'OK',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.voice_intake_requests (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    caller_phone text,
    caller_name  text,
    organization text,
    summary      text,
    status       text NOT NULL DEFAULT 'NEW',
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.sla_breaches (
    id       bigserial PRIMARY KEY,
    tenant   text NOT NULL,
    case_id  text NOT NULL,
    clock    text NOT NULL,        -- DETERMINATION_30BD | PAYMENT_30CD | ...
    detail   text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.audit_log (
    id        bigserial PRIMARY KEY,
    tenant    text NOT NULL,
    case_id   text,
    action    text NOT NULL,
    payload   text NOT NULL,
    prev_hash text NOT NULL,
    hash      text NOT NULL,       -- sha256(prev_hash || payload) — hash-chained
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Per-tenant DDL template (executed once per state with search_path set):
CREATE OR REPLACE FUNCTION public.provision_tenant(p_tenant text) RETURNS void AS $$
BEGIN
    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I', 'tenant_' || p_tenant);
    EXECUTE format($ddl$
        CREATE TABLE IF NOT EXISTS %I.cases (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            workflow_id text,
            case_number text NOT NULL UNIQUE,
            status text NOT NULL,
            status_reason text DEFAULT '',
            service_line text,
            plan_type text,
            qpa_cents bigint,
            -- NG phase 6: sector-generic twins (subject_line = "loss type",
            -- "parcel", "service line"…; benchmark_cents = estimate, assessed
            -- value, QPA…). Labels come from the manifest; legacy columns are
            -- kept populated for NSA-era readers and external reports.
            subject_line text,
            benchmark_cents bigint,
            provider_id text,
            payer_id text,
            open_negotiation_end date,
            offer_window_ends_at timestamptz,
            details jsonb NOT NULL DEFAULT '{}',  -- program-specific fields (LOB, disputed issue, OON, outcome, award…)
            assigned_to text,              -- keycloak sub of assignee
            assigned_role text,            -- CASE_MANAGER | ARBITRATOR
            batch_id uuid,
            parent_case_id uuid,
            duplicate_of uuid,
            opened_at timestamptz NOT NULL DEFAULT now(),
            updated_at timestamptz NOT NULL DEFAULT now()
        );
        -- Reproduced live: on a database tenant_<st>.cases already existed in
        -- (provisioned by an earlier deploy of an older schema version),
        -- CREATE TABLE IF NOT EXISTS above is a no-op, so these columns
        -- never actually get added -- the very next line then fails,
        -- "column assigned_to does not exist", because casemgmt-core.sql's
        -- own backfill for exactly this doesn't run until AFTER this file.
        -- Self-healing fix: do the same ADD COLUMN IF NOT EXISTS here too,
        -- so this function doesn't depend on file application order.
        ALTER TABLE %I.cases
            ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT '{}'::jsonb,
            ADD COLUMN IF NOT EXISTS assigned_to text,
            ADD COLUMN IF NOT EXISTS assigned_role text,
            ADD COLUMN IF NOT EXISTS batch_id uuid,
            ADD COLUMN IF NOT EXISTS parent_case_id uuid,
            ADD COLUMN IF NOT EXISTS duplicate_of uuid;
        CREATE INDEX IF NOT EXISTS cases_assignee ON %I.cases (assigned_to) WHERE assigned_to IS NOT NULL;
        CREATE TABLE IF NOT EXISTS %I.sealed_offers (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            case_id uuid NOT NULL,
            party_id text NOT NULL,
            nonce_b64 text NOT NULL,
            ciphertext_b64 text NOT NULL,   -- vault ciphertext; DB never sees plaintext
            wrapped_dek_b64 text NOT NULL,
            revealed boolean NOT NULL DEFAULT false,
            submitted_at timestamptz NOT NULL DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS %I.documents (
            id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
            case_id uuid NOT NULL,
            object_key text NOT NULL,       -- MinIO key (ciphertext object)
            size_bytes int,
            content_type text,
            sealed boolean NOT NULL DEFAULT false,
            uploaded_by text,
            version int NOT NULL DEFAULT 1,
            created_at timestamptz NOT NULL DEFAULT now()
        );
        CREATE TABLE IF NOT EXISTS %I.outbox (
            id bigserial PRIMARY KEY,
            topic text NOT NULL,
            key text NOT NULL,
            payload jsonb NOT NULL,
            published_at timestamptz,
            created_at timestamptz NOT NULL DEFAULT now()
        );
    $ddl$, 'tenant_'||p_tenant, 'tenant_'||p_tenant, 'tenant_'||p_tenant, 'tenant_'||p_tenant, 'tenant_'||p_tenant, 'tenant_'||p_tenant);
END;
$$ LANGUAGE plpgsql;

-- Provision all 50 states.
DO $$
DECLARE s text;
BEGIN
    FOREACH s IN ARRAY ARRAY['al','ak','az','ar','ca','co','ct','de','fl','ga','hi','id','il','in','ia',
                             'ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv','nh','nj',
                             'nm','ny','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','tx','ut','vt',
                             'va','wa','wv','wi','wy']
    LOOP
        PERFORM public.provision_tenant(s);
    END LOOP;
END $$;

-- Research-driven state configuration (SSL programs, citations, ambulance, all-payer).
INSERT INTO public.state_config (tenant, tb_ledger_id, ssl_program, ssl_citation, ssl_scope, ground_ambulance, all_payer_model) VALUES
  ('tx', 148, true,  'Tex. Ins. Code ch. 1271-1275', 'fully-insured HMO/PPO; state mediation', true,  false),
  ('ny', 136, true,  'NY PBFL art. 6 / IDR', 'fully-insured; NY IDR program', true, false),
  ('fl', 112, true,  'Fla. Stat. § 627.64194', 'fully-insured; state dispute resolution', false, false),
  ('nj', 134, true,  'NJ P.L. 2018, c.32 (OON Act)', 'fully-insured; arbitration program', true, false),
  ('md', 124, true,  'MD All-Payer Model (CMMI)', 'all-payer rate setting', false, true),
  ('de', 110, true,  '18 Del. C. § 3571F', 'fully-insured; arbitration', false, false),
  ('nm', 135, true,  'NMSA § 59A-57A', 'fully-insured; surprise billing act', false, false),
  ('wa', 153, true,  'RCW 48.49', 'fully-insured; Balance Billing Protection Act', true, false)
ON CONFLICT (tenant) DO NOTHING;
-- (remaining states default ssl_program=false → pure federal IDR routing)
