-- Author: Patrick Munis at newwave
-- Double-blind offer protocol (45 CFR 149.520(b); 2026 Final Rule).
--
-- Offers are sealed by vault-rs (AES-256-GCM, AAD = tenant:case:offer:party);
-- this table stores ONLY the cryptographic envelope plus submission metadata.
-- The offered amount exists in plaintext nowhere in Postgres until a lawful
-- reveal (BOTH_SUBMITTED | WINDOW_EXPIRED | RULE_DEFAULT, enforced by the
-- Temporal workflow and re-verified by the vault's reveal-check oracle).

CREATE TABLE IF NOT EXISTS public.offers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    case_id       text NOT NULL,
    party_id      text NOT NULL,              -- INITIATING | RESPONDING
    sealed_nonce  bytea NOT NULL,
    sealed_ct     bytea NOT NULL,
    wrapped_dek   bytea NOT NULL,
    submitted_by  text NOT NULL,              -- keycloak sub of the submitter
    submitted_at  timestamptz NOT NULL DEFAULT now(),
    revealed_at   timestamptz,
    revealed_amount_cents bigint,
    revealed_payload jsonb,                   -- decrypted justification/docs, post-reveal only
    reveal_reason text NOT NULL DEFAULT '',
    UNIQUE (tenant, case_id, party_id)        -- baseball-style: one offer per party
);

-- Written determination: the arbitrator selects ONE offer (no middle-ground
-- awards under the federal process). One determination per case.
CREATE TABLE IF NOT EXISTS public.determinations (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant            text NOT NULL,
    case_id           text NOT NULL UNIQUE,
    selected_offer_id uuid NOT NULL REFERENCES public.offers(id),
    amount_cents      bigint NOT NULL,        -- copied from the selected offer
    rationale         text NOT NULL,
    issued_by         text NOT NULL,
    issued_at         timestamptz NOT NULL DEFAULT now()
);

-- Specialty review pipeline: gives CODER / NURSE_PHYSICIAN (DOCTOR/NURSE) /
-- ATTORNEY persistent, queryable worklists instead of Temporal-only signals.
-- Completion forwards the matching workflow signal so the two systems never
-- diverge (CODING_REVIEW_COMPLETE / CLINICAL_REVIEW_COMPLETE /
-- ATTORNEY_REVIEW_COMPLETE).
CREATE TABLE IF NOT EXISTS public.specialty_reviews (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    discipline   text NOT NULL,               -- CODING | CLINICAL | LEGAL
    status       text NOT NULL DEFAULT 'REQUESTED',  -- REQUESTED | IN_REVIEW | COMPLETE
    note         text NOT NULL DEFAULT '',
    requested_by text NOT NULL,
    reviewer     text NOT NULL DEFAULT '',
    requested_at timestamptz NOT NULL DEFAULT now(),
    claimed_at   timestamptz,
    completed_at timestamptz
);
-- One active review per (case, discipline); completed reviews are history.
CREATE UNIQUE INDEX IF NOT EXISTS specialty_reviews_active_uq
    ON public.specialty_reviews (tenant, case_id, discipline)
    WHERE status <> 'COMPLETE';
CREATE INDEX IF NOT EXISTS specialty_reviews_queue_ix
    ON public.specialty_reviews (tenant, discipline, status);

-- Authenticated party workspace: maps Keycloak party accounts to the cases
-- they may act on. Created by CASE_MANAGER invite (Keycloak user provisioning
-- via the admin API); every party workspace/offer endpoint enforces this
-- mapping — a PARTY token alone grants nothing.
CREATE TABLE IF NOT EXISTS public.party_accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    party_id   text NOT NULL,                 -- INITIATING | RESPONDING
    kc_subject text NOT NULL,                 -- keycloak user id / JWT sub
    email      text NOT NULL,
    invited_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, kc_subject)
);
CREATE INDEX IF NOT EXISTS party_accounts_subject_ix
    ON public.party_accounts (tenant, kc_subject);
