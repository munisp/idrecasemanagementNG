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
