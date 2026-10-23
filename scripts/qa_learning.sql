-- Phase 3-lite: QA draft-quality capture.
--
-- Every QA decision (approve clean / approve after editing / reject) is a
-- domain expert grading a draft in context — the highest-quality supervision
-- signal the platform produces. Captured from day one as compact observations
-- (token counts + edit distance, NOT body text: qa_reviews already holds the
-- bodies, and duplicating correspondence into a second table doubles the
-- retention/FOIA surface for no analytic gain).
--
-- This table feeds GET /qa/draft-quality (per artifact/channel: approval
-- rate, unedited-approval rate, mean edit ratio) — the template-effectiveness
-- ranking that decides which templates and drafters actually work.

CREATE TABLE IF NOT EXISTS public.qa_edit_diffs (
    id            bigserial PRIMARY KEY,
    tenant        text NOT NULL,
    qa_id         text NOT NULL,
    case_id       text NOT NULL DEFAULT '',
    artifact      text NOT NULL DEFAULT '',   -- template key / copilot_* / letter artifact
    channel       text NOT NULL DEFAULT 'email',
    drafted_by    text NOT NULL DEFAULT '',
    decision      text NOT NULL,              -- APPROVE | REJECT
    edited        boolean NOT NULL DEFAULT false,
    draft_tokens  int NOT NULL DEFAULT 0,
    edit_distance int,                        -- token-level Levenshtein; NULL when unedited
    edit_ratio    double precision,           -- distance / draft_tokens; NULL when unedited
    decided_by    text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qa_edit_diffs_tenant_art ON public.qa_edit_diffs (tenant, artifact, created_at DESC);
CREATE INDEX IF NOT EXISTS qa_edit_diffs_case ON public.qa_edit_diffs (tenant, case_id);
