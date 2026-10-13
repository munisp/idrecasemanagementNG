-- ============================================================
-- TPA onboarding (tpa_onboarding.go, feature flag: tpa_onboarding)
--
-- Extends the TPA module from "registered org + client names" to a full
-- onboarding pipeline per initiating party:
--   profile (NPI/TIN/address) -> payment method (card or ACH, verified)
--   -> document package (6 slots, validated by doc-intel) -> READY to file.
--
-- A tpa_client's onboarding_status is derived, never set by hand:
--   PROFILE_INCOMPLETE -> PAYMENT_PENDING -> DOCS_PENDING -> READY
-- Filing (tpaIntake with package_id) is refused unless the client is READY
-- and the package's six slots are all ACCEPTED.
-- ============================================================

ALTER TABLE public.tpa_clients
    ADD COLUMN IF NOT EXISTS npi               text,   -- 10-digit, Luhn(80840-prefix) checked
    ADD COLUMN IF NOT EXISTS tin               text,   -- 9-digit EIN/SSN
    ADD COLUMN IF NOT EXISTS address           text,
    ADD COLUMN IF NOT EXISTS onboarding_status text NOT NULL DEFAULT 'PROFILE_INCOMPLETE';
    -- PROFILE_INCOMPLETE|PAYMENT_PENDING|DOCS_PENDING|READY

-- Payment rails the TPA sets up PER CLIENT (per initiating party). Card
-- numbers never touch this platform: processor_ref is the token/handle
-- returned by the payment processor; we keep brand + last4 for display.
-- ACH stores routing + account last4 plus the authorization capture.
CREATE TABLE IF NOT EXISTS public.tpa_payment_methods (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant        text NOT NULL,
    tpa_id        uuid NOT NULL REFERENCES public.tpas(id),
    client_id     uuid NOT NULL REFERENCES public.tpa_clients(id) ON DELETE CASCADE,
    method        text NOT NULL,             -- CARD|ACH
    label         text,                      -- "Operating account", "Corp card"...
    brand         text,                      -- VISA|MC|AMEX... (card only)
    last4         text NOT NULL,
    routing       text,                      -- ACH only, ABA checksum-validated
    processor_ref text,                      -- card token / ACH mandate handle
    auth_received boolean NOT NULL DEFAULT false,  -- ACH debit authorization on file
    verified      boolean NOT NULL DEFAULT false,  -- platform-side verification done
    status        text NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE|REVOKED
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tpa_payment_methods_client ON public.tpa_payment_methods (tenant, client_id);

-- Document packages: the 6 required documents for filing on behalf of one
-- initiating party. DISCRETE = six individual uploads; MERGED = one giant
-- PDF that doc-intel splits by page classification into the six slots.
CREATE TABLE IF NOT EXISTS public.tpa_packages (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant            text NOT NULL,
    tpa_id            uuid NOT NULL REFERENCES public.tpas(id),
    client_id         uuid NOT NULL REFERENCES public.tpa_clients(id) ON DELETE CASCADE,
    mode              text NOT NULL DEFAULT 'DISCRETE',  -- DISCRETE|MERGED
    status            text NOT NULL DEFAULT 'ASSEMBLING',
    -- ASSEMBLING|VALIDATING|READY|FILED|REJECTED
    source_object_key text,                -- MERGED: the sealed original PDF
    case_id           text,                -- set when filed
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tpa_packages_client ON public.tpa_packages (tenant, client_id);

-- One row per package slot (DISCRETE upload) or per split segment (MERGED).
-- doc-intel writes the verdict; reasons is a machine-readable list, e.g.
-- [{"code":"TYPE_MISMATCH","detail":"classified as idr_claim, expected eob"}].
CREATE TABLE IF NOT EXISTS public.tpa_package_docs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    package_id  uuid NOT NULL REFERENCES public.tpa_packages(id) ON DELETE CASCADE,
    slot        text NOT NULL,             -- EOB|ITEMIZED_BILL|PROOF_OF_COVERAGE|OPEN_NEGOTIATION_NOTICE|OFFER_SUBSTANTIATION|IDRE_ATTESTATION
    object_key  text NOT NULL,             -- vault-sealed object in idre-docs
    filename    text,
    status      text NOT NULL DEFAULT 'PENDING',  -- PENDING|ANALYZING|ACCEPTED|NEEDS_REVIEW|REJECTED
    reasons     jsonb NOT NULL DEFAULT '[]',
    page_start  int,                       -- MERGED segments: page range in source
    page_end    int,
    analyzed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (package_id, slot)
);
CREATE INDEX IF NOT EXISTS tpa_package_docs_pkg ON public.tpa_package_docs (tenant, package_id);
