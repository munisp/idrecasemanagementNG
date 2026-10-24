-- Author: Patrick Munis at newwave
-- ============================================================
-- Health plan onboarding (healthplan_onboarding.go, feature flag:
-- healthplan_onboarding) — the payer-side mirror of TPA onboarding.
--
-- A health plan registers as a first-class organization, claims itself in
-- the portal, rosters the parties it submits for (self-funded employer
-- groups, delegated provider networks/IPAs, ancillary facilities), stands
-- up a verified CARD/ACH payment rail PER PARTY, and assembles a validated
-- six-document package per party (discrete uploads or one merged PDF split
-- by doc-intel). Filing is gated on a READY package + verified payment.
--
-- Parallel tables to tpas (not a type flag on tpas): a TPA is an agent
-- company, a health plan is a regulated payer — different credentialing
-- (NAIC/state license/line-of-business), different RBAC, different audit
-- blast radius.
-- ============================================================

CREATE TABLE IF NOT EXISTS public.health_plans (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    name          text NOT NULL,
    naic          text,                    -- 5-digit NAIC company code
    fein          text,                    -- 9-digit federal EIN
    state_license text,                    -- license / certificate of authority
    line_of_business text,                 -- FULLY_INSURED|ASO|MEDICARE_ADVANTAGE|MEDICAID_MANAGED
    qpa_contact_email text,                -- QPA methodology contact
    contact_name  text,
    contact_email text NOT NULL,
    claim_code    text UNIQUE,             -- one-time, burned on claim
    status        text NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE|SUSPENDED
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, contact_email)
);

CREATE TABLE IF NOT EXISTS public.health_plan_users (
    tenant    text NOT NULL,
    user_sub  text NOT NULL,
    plan_id   uuid NOT NULL REFERENCES public.health_plans(id),
    linked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, user_sub)
);

-- Parties the plan submits for: employer groups (ASO), delegated networks,
-- facilities. onboarding_status is derived, never set by hand:
-- PROFILE_INCOMPLETE -> PAYMENT_PENDING -> DOCS_PENDING -> READY
CREATE TABLE IF NOT EXISTS public.hp_parties (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    plan_id       uuid NOT NULL REFERENCES public.health_plans(id),
    party_name    text NOT NULL,
    party_type    text NOT NULL DEFAULT 'EMPLOYER_GROUP',  -- EMPLOYER_GROUP|DELEGATED_NETWORK|FACILITY
    contact_email text,
    npi           text,                    -- required for DELEGATED_NETWORK/FACILITY
    tin           text,
    address       text,
    status        text NOT NULL DEFAULT 'ACTIVE',
    onboarding_status text NOT NULL DEFAULT 'PROFILE_INCOMPLETE',
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, plan_id, party_name)
);

-- Per-party payment rails. Card numbers never touch this platform:
-- processor_ref is the token; ACH stores routing + last4 + auth capture.
CREATE TABLE IF NOT EXISTS public.hp_payment_methods (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    plan_id       uuid NOT NULL REFERENCES public.health_plans(id),
    party_id      uuid NOT NULL REFERENCES public.hp_parties(id) ON DELETE CASCADE,
    method        text NOT NULL,             -- CARD|ACH
    label         text,
    brand         text,
    last4         text NOT NULL,
    routing       text,
    processor_ref text,
    auth_received boolean NOT NULL DEFAULT false,
    verified      boolean NOT NULL DEFAULT false,
    status        text NOT NULL DEFAULT 'ACTIVE',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hp_payment_methods_party ON public.hp_payment_methods (tenant, party_id);

CREATE TABLE IF NOT EXISTS public.hp_packages (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant            text NOT NULL,
    plan_id           uuid NOT NULL REFERENCES public.health_plans(id),
    party_id          uuid NOT NULL REFERENCES public.hp_parties(id) ON DELETE CASCADE,
    mode              text NOT NULL DEFAULT 'DISCRETE',  -- DISCRETE|MERGED
    status            text NOT NULL DEFAULT 'ASSEMBLING',
    -- ASSEMBLING|VALIDATING|READY|FILED|REJECTED
    source_object_key text,
    case_id           text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hp_packages_party ON public.hp_packages (tenant, party_id);

CREATE TABLE IF NOT EXISTS public.hp_package_docs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    package_id  uuid NOT NULL REFERENCES public.hp_packages(id) ON DELETE CASCADE,
    slot        text NOT NULL,             -- same six slots as TPA packages
    object_key  text NOT NULL,
    filename    text,
    status      text NOT NULL DEFAULT 'PENDING',  -- PENDING|ANALYZING|ACCEPTED|NEEDS_REVIEW|REJECTED
    reasons     jsonb NOT NULL DEFAULT '[]',
    page_start  int,
    page_end    int,
    analyzed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (package_id, slot)
);
CREATE INDEX IF NOT EXISTS hp_package_docs_pkg ON public.hp_package_docs (tenant, package_id);

-- Origin: attribute plan-filed disputes to plan + party alongside the
-- existing TPA attribution. NG's case_origin was TPA-only (tpa_id/client_id
-- NOT NULL) — relax both and require at least one agent.
ALTER TABLE public.case_origin ALTER COLUMN tpa_id DROP NOT NULL;
ALTER TABLE public.case_origin ALTER COLUMN client_id DROP NOT NULL;
ALTER TABLE public.case_origin
    ADD COLUMN IF NOT EXISTS health_plan_id uuid REFERENCES public.health_plans(id),
    ADD COLUMN IF NOT EXISTS hp_party_id    uuid REFERENCES public.hp_parties(id);
