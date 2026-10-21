-- deid.sql — persistence for the de-identification service (services/deid-py).
--
-- deid_reports: one row per scan/deidentify/block decision on an artifact
-- that left (or was held from leaving) the platform. The input text is NEVER
-- stored — only its SHA-256, the policy/detector versions, per-type counts,
-- and the finding spans. This is the answer to "what went out, scrubbed how,
-- under which policy?" for auditors.
--
-- document_clinical: Phase 2 — clinical entities (drug/condition/anatomy/
-- procedure) extracted from document text by deid-py for doc-intel. Feeds
-- screening context and the clinical-overlap duplicate signal.

CREATE TABLE IF NOT EXISTS public.deid_reports (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    case_id     text,
    artifact    text NOT NULL,              -- letter|email|package|lakehouse|document
    ref_id      text,                       -- qa_reviews id, sync run id, …
    action      text NOT NULL,              -- SCAN|DEIDENTIFY|BLOCK|CLINICAL
    policy      text NOT NULL,
    detectors   jsonb NOT NULL DEFAULT '[]',
    counts      jsonb NOT NULL DEFAULT '{}',
    findings    jsonb NOT NULL DEFAULT '[]',
    risk        numeric NOT NULL DEFAULT 0,
    text_hash   text NOT NULL,
    output_hash text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deid_reports_case ON public.deid_reports (tenant, case_id, created_at DESC);
CREATE INDEX IF NOT EXISTS deid_reports_artifact ON public.deid_reports (tenant, artifact, created_at DESC);

CREATE TABLE IF NOT EXISTS public.document_clinical (
    tenant      text NOT NULL,
    doc_id      text NOT NULL,
    case_id     text,
    entities    jsonb NOT NULL DEFAULT '[]',  -- [{type,start,end,score,text,detector}]
    summary     jsonb NOT NULL DEFAULT '{}',  -- {CONDITION: 3, DRUG: 2, …}
    detectors   jsonb NOT NULL DEFAULT '[]',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, doc_id)
);
CREATE INDEX IF NOT EXISTS document_clinical_case ON public.document_clinical (tenant, case_id);
