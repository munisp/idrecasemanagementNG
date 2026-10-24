-- Author: Patrick Munis at newwave
-- Document review queue: ANALYZED_WITH_FINDINGS is set by doc-intel when
-- analysis surfaces something a human should check, but no transition ever
-- cleared it — the review_documents next-action and the review inbox need an
-- explicit acknowledge. Reviewer attribution lives on the row (who checked
-- the findings and when), not in the JSON result.

ALTER TABLE public.doc_analysis
    ADD COLUMN IF NOT EXISTS reviewed_by text,
    ADD COLUMN IF NOT EXISTS reviewed_at timestamptz;

-- The inbox query: findings still waiting, newest statutory pressure first.
CREATE INDEX IF NOT EXISTS doc_analysis_review_queue
    ON public.doc_analysis (tenant, analyzed_at DESC)
    WHERE status = 'ANALYZED_WITH_FINDINGS';
