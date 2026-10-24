-- Author: Patrick Munis at newwave
-- Case status transitions: structured capture for the transition-matrix
-- model. Until now a case's status history existed only inside free-text
-- activity bodies -- fine for reading, unfit for computing dwell times and
-- path shares. This table is written by the API at every status write site
-- (initiation, manual program update, bulk, auto-advance) so the matrix
-- reads one clean fact table.
--
-- Two axes share the table because the platform runs two parallel status
-- vocabularies:
--   lifecycle -- tenant_X.cases.status, the canonical federal NSA lifecycle
--                (INITIATED .. OPEN_NEGOTIATION .. DETERMINED .. CLOSED_%)
--   program   -- tenant_X.cases.internal_status, the manifest-defined
--                working board (canonicalised against program_rules)
-- They are never mixed in one chain: every row names its axis and the
-- matrix is computed per axis.

CREATE TABLE IF NOT EXISTS public.case_transitions (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    case_id     text NOT NULL,
    axis        text NOT NULL,            -- lifecycle | program
    from_status text,                     -- NULL on the opening transition
    to_status   text NOT NULL,
    actor_sub   text,                     -- NULL for machine actors
    source      text NOT NULL,            -- status_update | bulk | auto_advance | initiation | backfill
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS case_transitions_tenant_case
    ON public.case_transitions (tenant, case_id, axis, created_at, id);

CREATE INDEX IF NOT EXISTS case_transitions_tenant_axis
    ON public.case_transitions (tenant, axis, created_at);

-- Backfill from the activity trail (runs once: skipped when the table
-- already has rows). STATUS_CHANGE bodies carry the new status in one of
-- two fixed shapes:
--   "Status changed to <X> (bulk) by <sub>"        -> lifecycle axis
--   "Status updated by <sub> — internal: <X>, agency: <Y>" -> program axis
-- ("-" = unchanged side, skipped). The from-side of each hop is the
-- previous hop's to-side within the same case and axis.
WITH src AS (
    SELECT tenant, case_id,
           CASE WHEN body LIKE 'Status changed to % (bulk) by %' THEN 'lifecycle' ELSE 'program' END AS axis,
           coalesce(nullif(substring(body from 'changed to (.*?) \(bulk\) by '), ''),
                    nullif(substring(body from 'internal: (.*?), agency:'), '')) AS to_status,
           created_at, id
    FROM public.case_activities
    WHERE type = 'STATUS_CHANGE'
      AND NOT EXISTS (SELECT 1 FROM public.case_transitions)
), valid AS (
    -- Filter BEFORE the lag: an agency-only hop ("internal: -") records no
    -- program transition, and if it stayed in the window it would corrupt
    -- the next hop's from_status.
    SELECT * FROM src WHERE to_status IS NOT NULL AND to_status <> '-'
), chained AS (
    SELECT tenant, case_id, axis,
           lag(to_status) OVER w AS from_status,
           to_status, created_at
    FROM valid
    WINDOW w AS (PARTITION BY tenant, case_id, axis ORDER BY created_at, id)
)
INSERT INTO public.case_transitions
    (tenant, case_id, axis, from_status, to_status, actor_sub, source, created_at)
SELECT tenant, case_id, axis, from_status, to_status, NULL, 'backfill', created_at
FROM chained
WHERE from_status IS NULL OR from_status <> to_status;
