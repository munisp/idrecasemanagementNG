-- Author: Patrick Munis at newwave
-- doc-intel continuous learning: human corrections + learned patterns.
-- Apply after onboarding-and-docintel.sql (doc_feedback references doc_analysis).

-- Every human correction of an extracted field, captured from the review UI.
-- This is the training signal for the learning loop; rows are never updated
-- (each correction is a fact; re-corrections are new rows).
CREATE TABLE IF NOT EXISTS public.doc_feedback (
    id               bigserial PRIMARY KEY,
    tenant           text        NOT NULL,
    doc_id           uuid        NOT NULL REFERENCES public.doc_analysis(doc_id) ON DELETE CASCADE,
    doc_type         text        NOT NULL DEFAULT '',
    field            text        NOT NULL,
    extracted_value  text,
    corrected_value  text,
    source           text        NOT NULL DEFAULT 'review',  -- review | bulk-import | qa-sample
    created_by       text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS doc_feedback_doc_idx    ON public.doc_feedback (doc_id);
CREATE INDEX IF NOT EXISTS doc_feedback_field_idx  ON public.doc_feedback (field);

-- Mined extraction patterns. CANDIDATE = observed but unproven; ACTIVE =
-- promoted by champion/challenger evaluation (precision >= threshold over the
-- feedback corpus) and applied by stage_fields; RETIRED = automatically
-- rolled back when precision degraded. version increments on each promotion,
-- so the applied set is auditable and reversible by status flip.
CREATE TABLE IF NOT EXISTS public.doc_learned_patterns (
    id           bigserial PRIMARY KEY,
    pattern_type text        NOT NULL,           -- LABEL_SYNONYM
    key          text        NOT NULL,           -- canonical field (billed, allowed, paid, ...)
    value        text        NOT NULL,           -- learned label phrase
    support      int         NOT NULL DEFAULT 1, -- distinct documents the pattern was harvested from
    precision    numeric(5,4),                   -- champion/challenger score over the feedback corpus
    status       text        NOT NULL DEFAULT 'CANDIDATE'
                 CHECK (status IN ('CANDIDATE','ACTIVE','RETIRED')),
    version      int         NOT NULL DEFAULT 0,
    first_seen   timestamptz NOT NULL DEFAULT now(),
    last_seen    timestamptz NOT NULL DEFAULT now(),
    promoted_at  timestamptz,
    UNIQUE (pattern_type, key, value)
);

-- Physical-check corrections. Checks are not documents (no doc_analysis row),
-- so they get their own feedback table keyed to public.checks. Corrections to
-- the memo line and payer name feed mined patterns the check processor applies
-- on the next extraction (memo noise stripping, payer-name aliasing).
CREATE TABLE IF NOT EXISTS public.check_feedback (
    id               bigserial PRIMARY KEY,
    tenant           text        NOT NULL,
    check_id         uuid        NOT NULL REFERENCES public.checks(id) ON DELETE CASCADE,
    field            text        NOT NULL,   -- memo | payer_name | check_date | check_number | routing_number | account_number | amount
    extracted_value  text,
    corrected_value  text,
    source           text        NOT NULL DEFAULT 'review',
    created_by       text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS check_feedback_check_idx  ON public.check_feedback (check_id);
CREATE INDEX IF NOT EXISTS check_feedback_field_idx  ON public.check_feedback (field);
