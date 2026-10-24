-- Author: Patrick Munis at newwave
-- ShareBox resumable-transfer state.
-- Resumable uploads use a tus-style protocol over MinIO multipart with
-- per-part vault sealing: each 8–32MB part is encrypted independently, so
-- downloads can decrypt and stream only the parts overlapping a requested
-- byte range (true Range support on ciphertext at rest).

CREATE TABLE IF NOT EXISTS public.share_uploads (
    upload_id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token           text NOT NULL REFERENCES public.share_links(token),
    tenant          text NOT NULL,
    case_id         text NOT NULL,
    filename        text NOT NULL,
    size_bytes      bigint NOT NULL,           -- declared total (Upload-Length)
    object_key      text NOT NULL,
    minio_upload_id text NOT NULL,
    bytes_rcvd      bigint NOT NULL DEFAULT 0, -- bytes received so far
    parts           jsonb NOT NULL DEFAULT '[]', -- [{n, etag, plain_bytes, sealed_bytes}]
    status          text NOT NULL DEFAULT 'OPEN', -- OPEN|COMPLETE|ABORTED
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS share_uploads_token ON public.share_uploads (token, status);

-- Documents uploaded in sealed parts carry their manifest on the row; every
-- stored document carries its malware-scan verdict (PENDING|CLEAN|INFECTED).
DO $$
DECLARE s text;
BEGIN
    FOREACH s IN ARRAY ARRAY['tx','ca','ny','fl','al','ak','az','ar','co','ct','de','ga','hi','id','il',
                             'in','ia','ks','ky','la','me','md','ma','mi','mn','ms','mo','mt','ne','nv',
                             'nh','nj','nm','nc','nd','oh','ok','or','pa','ri','sc','sd','tn','ut','vt',
                             'va','wa','wv','wi','wy']
    LOOP
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS parts jsonb', 'tenant_'||s);
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS scan_status text NOT NULL DEFAULT ''PENDING''', 'tenant_'||s);
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS scan_detail text', 'tenant_'||s);
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS filename text', 'tenant_'||s);
        EXECUTE format('ALTER TABLE %I.documents ADD COLUMN IF NOT EXISTS folder text NOT NULL DEFAULT ''GENERAL''', 'tenant_'||s);
    END LOOP;
END $$;
