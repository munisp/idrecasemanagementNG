-- Author: Patrick Munis at newwave
-- seed-honeytokens.sql — canary records for breach detection.
--
-- One canary case per tenant schema (tenant_<st>). Canary rows are real
-- rows (so they surface in list/search exactly like any case) with three
-- properties:
--   1. case_number = 'CANARY-<tenant>'   (deterministic, greppable)
--   2. zero legitimate traffic           (never assigned, never routed)
--   3. every access is audited           (logAudit hash-chain + SIEM)
--
-- Any read of a canary row is by definition suspicious: no workflow ever
-- touches one. Wazuh rules 100200-100202 (deploy/wazuh/rules/idre-rules.xml)
-- fire on audit_log rows referencing a CANARY case number, on storage
-- gateway GETs of canary-* object keys, and on search hits for the token.
--
-- Idempotent. Run once, and again after onboarding each new tenant:
--   psql "$DATABASE_URL" -f scripts/seed-honeytokens.sql

DO $$
DECLARE
    st text;
    cid uuid;
BEGIN
    FOREACH st IN ARRAY ARRAY['al','ak','az','ar','ca','co','ct','de','fl','ga','hi','id','il','in','ia',
                              'ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv','nh','nj',
                              'nm','ny','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','tx','ut','vt',
                              'va','wa','wv','wi','wy']
    LOOP
        EXECUTE format($i$
            INSERT INTO %I.cases (case_number, status, service_line, details)
            VALUES ($1, 'OPEN', 'CANARY',
                    jsonb_build_object('canary', true,
                                       'purpose', 'breach detection — any access is an incident',
                                       'seeded_at', now()::text))
            ON CONFLICT (case_number) DO NOTHING
            RETURNING id
        $i$, 'tenant_' || st) USING 'CANARY-' || st INTO cid;

        IF cid IS NOT NULL THEN
            -- Matching canary document row: the 'canary-*' object key pattern
            -- is what the storage-gateway SIEM rule matches, so canary reads
            -- at the object layer stay visible too. size 0 — no real object
            -- is ever written; a 404 on fetch is itself a detection signal.
            EXECUTE format($i$
                INSERT INTO %I.documents (case_id, object_key, size_bytes, content_type, uploaded_by)
                VALUES ($1, $2, 0, 'application/pdf', 'system:honeytoken')
                ON CONFLICT DO NOTHING
            $i$, 'tenant_' || st)
            USING cid, st || '/cases/' || cid || '/canary-' || st || '.enc';
        END IF;
    END LOOP;
END $$;
