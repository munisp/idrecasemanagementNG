-- CRM core objects (Twenty/Salesforce-parity layer), tenant-scoped.

-- Accounts: organizations (provider, payer, IDRE, auditor, vendor).
CREATE TABLE IF NOT EXISTS public.accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    type       text NOT NULL,        -- PROVIDER | PAYER | IDRE | AUDITOR | OTHER
    legal_name text NOT NULL,
    ein_masked text,
    npi        text,
    website    text,
    phone      text,
    address    jsonb NOT NULL DEFAULT '{}',
    custom     jsonb NOT NULL DEFAULT '{}',   -- custom fields (Salesforce-style)
    onboarding_application_id uuid,           -- traceability to onboarding
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS accounts_tenant_type ON public.accounts (tenant, type);

-- Contacts: people at accounts (billing lead, claims rep, arbitrator, auditor).
CREATE TABLE IF NOT EXISTS public.contacts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    account_id uuid REFERENCES public.accounts(id) ON DELETE SET NULL,
    name       text NOT NULL,
    role_title text,
    email      text,
    phone      text,
    keycloak_sub text,                          -- linked portal user, if any
    custom     jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS contacts_account ON public.contacts (account_id);

-- Leads: unqualified intake — voice calls, web forms, referrals. Convert to
-- account (+ optionally a case) when qualified.
CREATE TABLE IF NOT EXISTS public.leads (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    source     text NOT NULL,        -- VOICE | WEB | REFERRAL | MANUAL
    name       text,
    organization text,
    phone      text,
    email      text,
    summary    text,
    status     text NOT NULL DEFAULT 'NEW',   -- NEW | WORKING | CONVERTED | DISQUALIFIED
    converted_account_id uuid,
    voice_intake_id bigint,                    -- link back to voice intake
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS leads_tenant_status ON public.leads (tenant, status);

-- Tasks: assignable work items on cases/accounts/leads with due dates.
CREATE TABLE IF NOT EXISTS public.tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    subject     text NOT NULL,
    case_id     text,
    account_id  uuid,
    lead_id     uuid,
    assignee    text,               -- keycloak sub
    due_date    date,
    status      text NOT NULL DEFAULT 'OPEN',   -- OPEN | DONE | CANCELLED
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tasks_assignee ON public.tasks (tenant, assignee, status);
-- Completion timestamp for throughput reporting (ops dashboard trend);
-- backfill-safe: trend queries COALESCE(completed_at, created_at).
ALTER TABLE public.tasks ADD COLUMN IF NOT EXISTS completed_at timestamptz;

-- Notes: free-form on any record (case, account, lead).
CREATE TABLE IF NOT EXISTS public.notes (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    record_type text NOT NULL,      -- CASE | ACCOUNT | LEAD
    record_id  text NOT NULL,
    stream     text NOT NULL DEFAULT 'internal', -- internal|coder|clinical|legal|external_agency (program: notes_streams)
    body       text NOT NULL,
    author     text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notes_record ON public.notes (tenant, record_type, record_id);
-- Same class of bug as saved_views.pinned (casemgmt-core.sql): CREATE TABLE
-- IF NOT EXISTS is a no-op against a public.notes that already existed from
-- an earlier deploy of a schema version before `stream` was added.
ALTER TABLE public.notes ADD COLUMN IF NOT EXISTS stream text NOT NULL DEFAULT 'internal';
