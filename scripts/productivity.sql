-- productivity.sql — interaction telemetry + learning substrate (Phase 1 of
-- the productivity program: measure, then prefill/recommend).
--
-- interaction_events: append-only portal interaction stream. Privacy rules
-- are enforced at the emitter (portal/js/telemetry.js): keystrokes are
-- COUNTED per field, never captured; targets are control labels, never
-- values. This table is the baseline for "clicks per workflow" and the
-- training data for the next-action recommender (accept vs override events)
-- and, later, the calibrated decision layer.

CREATE TABLE IF NOT EXISTS public.interaction_events (
    id          bigserial PRIMARY KEY,
    tenant      text NOT NULL,
    actor_sub   text NOT NULL,
    session_id  text NOT NULL,
    workflow    text NOT NULL,      -- case_initiate | qa_queue | case_detail | crm | dashboard | other
    action      text NOT NULL,      -- click | keystroke | submit | navigate |
                                    -- prefill_accept | prefill_override |
                                    -- next_action_accept | next_action_override
    target      text NOT NULL DEFAULT '',
    meta        jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS interaction_events_wf ON public.interaction_events (tenant, workflow, created_at DESC);
CREATE INDEX IF NOT EXISTS interaction_events_actor ON public.interaction_events (tenant, actor_sub, created_at DESC);
