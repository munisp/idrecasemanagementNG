-- ============================================================
-- Entity 360 (entity360.go) — one registry row per real-world
-- organization, with links back to every place the platform has met it:
-- a CRM account, a TPA, a TPA's client, a health plan, a plan's party.
-- The 360 endpoint aggregates across those sources, so a provider that a
-- TPA files for, that also has a CRM account, is ONE entity — not three
-- partial records. Feature-flagged: entity360.
-- Safe to re-run.
-- ============================================================

-- The canonical organization. Identity keys (npi / tin / naic / fein) are
-- what the resolver matches on; any of them may be empty (an employer
-- group has no NPI, a provider has no NAIC). Empty never matches.
CREATE TABLE IF NOT EXISTS public.entities (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant         text NOT NULL,
    entity_type    text NOT NULL,        -- TPA | PROVIDER | HEALTH_PLAN | EMPLOYER_GROUP | DELEGATED_NETWORK | FACILITY | OTHER
    canonical_name text NOT NULL,
    npi            text NOT NULL DEFAULT '',
    tin            text NOT NULL DEFAULT '',
    naic           text NOT NULL DEFAULT '',
    fein           text NOT NULL DEFAULT '',
    status         text NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE | MERGED | ARCHIVED
    created_by     text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
-- An identity key belongs to at most one entity per tenant: two rows with
-- the same NPI are the same organization by definition, so the resolver
-- merges rather than duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS entities_npi_uniq  ON public.entities (tenant, npi)  WHERE npi <> '';
CREATE UNIQUE INDEX IF NOT EXISTS entities_tin_uniq  ON public.entities (tenant, tin)  WHERE tin <> '';
CREATE UNIQUE INDEX IF NOT EXISTS entities_naic_uniq ON public.entities (tenant, naic) WHERE naic <> '';
CREATE INDEX IF NOT EXISTS entities_name ON public.entities (tenant, lower(canonical_name));
CREATE INDEX IF NOT EXISTS entities_tenant_type ON public.entities (tenant, entity_type);

-- Where the platform has met the entity. A source row links to exactly one
-- entity (UNIQUE on source); an entity gathers many source rows.
--   link_kind ID_MATCH    identity key matched — no human needed
--             CONFIRMED   a person linked it (fuzzy name match or override)
CREATE TABLE IF NOT EXISTS public.entity_links (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    entity_id    uuid NOT NULL REFERENCES public.entities(id) ON DELETE CASCADE,
    source_table text NOT NULL,   -- accounts | tpas | tpa_clients | health_plans | hp_parties
    source_id    text NOT NULL,
    link_kind    text NOT NULL,   -- ID_MATCH | CONFIRMED
    linked_by    text NOT NULL DEFAULT '',   -- keycloak sub; empty for ID_MATCH
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, source_table, source_id)
);
CREATE INDEX IF NOT EXISTS entity_links_entity ON public.entity_links (tenant, entity_id);

-- CRM accounts carry their resolved entity (set on create/update when the
-- account has an NPI or EIN; also settable from the 360 screen).
ALTER TABLE public.accounts ADD COLUMN IF NOT EXISTS entity_id uuid REFERENCES public.entities(id);
CREATE INDEX IF NOT EXISTS accounts_entity ON public.accounts (tenant, entity_id) WHERE entity_id IS NOT NULL;

-- Merge (#1): a retired entity points at the survivor.
ALTER TABLE public.entities ADD COLUMN IF NOT EXISTS merged_into uuid REFERENCES public.entities(id);
CREATE INDEX IF NOT EXISTS entities_merged ON public.entities (tenant, merged_into) WHERE merged_into IS NOT NULL;

-- Trigram similarity: suggestion candidates come from similarity() when a
-- plain substring match finds too little — "Gulf Coast ER Physicians"
-- shares no substring with the canonical row.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS entities_name_trgm ON public.entities USING gin (canonical_name gin_trgm_ops);

-- Tenant-wide case statistics: the 360 benchmark reads this cache
-- (refreshed by the entity backfill job and opportunistically on a live
-- compute) instead of scanning every closed case per view.
CREATE TABLE IF NOT EXISTS public.tenant_case_stats (
    tenant         text PRIMARY KEY,
    closed_cases   bigint NOT NULL DEFAULT 0,
    avg_days       numeric,
    median_days    numeric,
    above_qpa_pct  numeric,
    computed_at    timestamptz NOT NULL DEFAULT now()
);
