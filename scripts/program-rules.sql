-- Program rules + FL AHCA extension tables.
-- One row per tenant in public.program_rules; the JSONB config drives clocks,
-- statuses, eligibility, numbering, fees, escalation, correspondence, and
-- deliverables. A state customizes its program by editing config — no code.

CREATE TABLE IF NOT EXISTS public.program_rules (
    tenant     text PRIMARY KEY,
    program    text NOT NULL,
    config     jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Program-specific dates/statuses ride on the case row.
ALTER TABLE tenant_tx.cases ADD COLUMN IF NOT EXISTS internal_status text;
-- (all tenant schemas get the same columns via the loop below)
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['tx','ca','ny','fl'] LOOP
    EXECUTE format('ALTER TABLE tenant_%I.cases
        ADD COLUMN IF NOT EXISTS internal_status text,
        ADD COLUMN IF NOT EXISTS agency_status text,
        ADD COLUMN IF NOT EXISTS disputed_amount_cents bigint,
        ADD COLUMN IF NOT EXISTS num_claims int,
        ADD COLUMN IF NOT EXISTS program_dates jsonb NOT NULL DEFAULT ''{}''::jsonb', t);
  END LOOP;
END $$;

-- QA gate: every outbound letter/email is a record with approver + decision.
CREATE TABLE IF NOT EXISTS public.qa_reviews (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    artifact     text NOT NULL,        -- template key, e.g. acceptance_provider
    channel      text NOT NULL DEFAULT 'email',
    subject      text,
    body         text NOT NULL,        -- rendered merge output
    to_recipients   jsonb NOT NULL DEFAULT '[]',
    cc_recipients   jsonb NOT NULL DEFAULT '[]',
    status       text NOT NULL DEFAULT 'PENDING',  -- PENDING|APPROVED|REJECTED|SENT
    drafted_by   text NOT NULL,
    reviewed_by  text,
    review_note  text,
    reviewed_at  timestamptz,
    sent_at      timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS qa_reviews_case ON public.qa_reviews (tenant, case_id);
CREATE INDEX IF NOT EXISTS qa_reviews_queue ON public.qa_reviews (tenant, status);

-- Correspondence log: what actually went out (replaces the Excel Inquiries Log).
CREATE TABLE IF NOT EXISTS public.correspondence_log (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    direction  text NOT NULL DEFAULT 'OUT',  -- OUT|IN
    channel    text NOT NULL DEFAULT 'email',
    template   text,
    recipients jsonb NOT NULL DEFAULT '{}',  -- {to:[],cc:[]}
    subject    text,
    body       text,
    sent_by    text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS correspondence_case ON public.correspondence_log (tenant, case_id);

-- Invoices: one invoice number (= case number) can carry two receivables.
CREATE TABLE IF NOT EXISTS public.invoices (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    invoice_no   text NOT NULL,               -- equals case_number per program convention
    party        text NOT NULL,               -- HEALTH_PLAN|PROVIDER
    kind         text NOT NULL DEFAULT 'FULL_REVIEW', -- INITIAL_FEE|FULL_REVIEW|DEFAULT
    amount_cents bigint NOT NULL,
    status       text NOT NULL DEFAULT 'OPEN',-- OPEN|PAID|REFUNDED|VOID
    due_date     date,
    paid_at      date,
    remittance_ref text,                      -- RA/ERA reference
    created_by   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, party, kind)
);

-- Claim-level sub-records for large-volume disputes (G10).
CREATE TABLE IF NOT EXISTS public.case_claims (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    case_id      text NOT NULL,
    claim_number text NOT NULL,
    cpt          text,
    billed_cents bigint,
    paid_cents   bigint,
    status       text NOT NULL DEFAULT 'SUBMITTED',
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant, case_id, claim_number)
);
CREATE INDEX IF NOT EXISTS case_claims_case ON public.case_claims (tenant, case_id);

-- Pre-case intake: instructions requested before any case exists (G9/G12).
CREATE TABLE IF NOT EXISTS public.intake_requests (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant      text NOT NULL,
    email       text NOT NULL,
    contact_name text,
    org         text,
    status      text NOT NULL DEFAULT 'INSTRUCTED', -- INSTRUCTED|DOCS_RECEIVED|PAID|CONVERTED|CLOSED_REFUNDED
    case_id     text,                               -- set at conversion
    outreach_at timestamptz,                        -- refund clock basis
    notes       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
-- AHCA answers 2026: intake carries the filing party type (a health plan CAN
-- file, though rare) and the packet-complete timestamp that anchors the
-- 10-day initial-review clock (the clock runs from COMPLETE packet receipt,
-- not payment or submission). Backfilled for pre-existing databases.
ALTER TABLE public.intake_requests
    ADD COLUMN IF NOT EXISTS filing_party_type text NOT NULL DEFAULT 'PROVIDER', -- PROVIDER|HEALTH_PLAN
    ADD COLUMN IF NOT EXISTS packet_complete_at timestamptz;                     -- null = awaiting documents

-- Rule-engine audit trail (append-only by design — no UPDATE/DELETE path).
-- Every rules write stores the full before/after config plus actor + note.
CREATE TABLE IF NOT EXISTS public.rule_changes (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    changed_by text NOT NULL,              -- keycloak sub of the admin
    note       text,                       -- change rationale from the UI
    before     jsonb NOT NULL,
    after      jsonb NOT NULL,
    changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS rule_changes_tenant ON public.rule_changes (tenant, id DESC);

-- Rule quarantine for onboarding uploads: doc.upload rules fire on the
-- pre-case path too, and block_request needs somewhere to land. doc-intel
-- skips documents in this state (is_blocked pre-flight in main.py).
ALTER TABLE public.application_documents
    ADD COLUMN IF NOT EXISTS analysis_status text NOT NULL DEFAULT 'QUEUED'; -- QUEUED|BLOCKED|...

-- Tokenized party document links (ShareFile replacement).
CREATE TABLE IF NOT EXISTS public.share_links (
    token      text PRIMARY KEY,              -- URL-safe random
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    kind       text NOT NULL,                 -- upload|download
    object_key text,                          -- download: specific doc
    expires_at timestamptz NOT NULL,
    uses       int NOT NULL DEFAULT 0,
    max_uses   int NOT NULL DEFAULT 1,
    files_used int NOT NULL DEFAULT 0,        -- abuse budgets (ShareBox)
    max_files  int NOT NULL DEFAULT 5,
    bytes_used bigint NOT NULL DEFAULT 0,
    max_bytes  bigint NOT NULL DEFAULT 524288000, -- 500MB per link
    created_by text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Eligibility results: computed per case from program rules + evidence.
CREATE TABLE IF NOT EXISTS public.eligibility_reviews (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    result     text NOT NULL,                 -- ELIGIBLE|INELIGIBLE|HOLD_AOR
    reason     text,                          -- reason code from program config
    evidence   jsonb NOT NULL DEFAULT '{}',   -- per-criterion findings
    decided_by text,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Plan opt-out adjudication (G14).
CREATE TABLE IF NOT EXISTS public.opt_out_decisions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant     text NOT NULL,
    case_id    text NOT NULL,
    eligible   boolean NOT NULL,
    rationale  text NOT NULL,
    decided_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Contract deliverables calendar (G7 second half).
CREATE TABLE IF NOT EXISTS public.deliverables (
    id         bigserial PRIMARY KEY,
    tenant     text NOT NULL,
    name       text NOT NULL,
    contract_ref text,
    due_rule   text NOT NULL,                 -- e.g. weekly:MONDAY | monthly:10 | case:AGENCY_RECOMMENDATION
    case_id    text,                          -- set for per-case deliverables
    due_date   date,
    status     text NOT NULL DEFAULT 'OPEN',  -- OPEN|DELIVERED|LATE
    delivered_at timestamptz,
    UNIQUE (tenant, name, case_id)
);

-- ---------------------------------------------------------------------------
-- Seed: Florida AHCA CDR program profile.
-- ---------------------------------------------------------------------------
INSERT INTO public.program_rules (tenant, program, config) VALUES ('fl', 'FL AHCA CDR', $$
{
  "case_number": {"pattern": "FL{yy}-{seq}", "seq_pad": 3},
  "statuses": {
    "internal": ["Initial Review Pending","QA Initial Review","Provider Acceptance Letter Issued","Provider Closure Letter Issued","RFI Pending Provider Response","QA Letter","Plan Notification Packet Issued","Review In Progress","RFI Pending Plan Response","Plan - No Response","Plan Opt-Out","Provider - Withdrawal","Hold","QA Final Determination","Determination sent to FL","Final Order Issued","Decided - Invoice Paid","Dismissed","Withdrawn","Ineligible"],
    "agency": ["Pending Initial Review","Awaiting Provider Response","Awaiting Plan Response","Under Review","Decided","Closed","Other"]
  },
  "clocks": [
    {"name":"AGENCY_RECOMMENDATION","label":"Recommendation to the Agency","basis":"received_at","days":60,"day_type":"calendar","cite":"AHCA CDR contract §2.3.3","breach":"Contract SLA breach"},
    {"name":"INITIAL_REVIEW","label":"Initial eligibility review","basis":"packet_complete_at","days":10,"day_type":"calendar","cite":"Contract §2.3.4","breach":"Internal SLA breach"},
    {"name":"PROVIDER_PERMISSION","label":"Provider permission after estimate","basis":"estimate_sent_at","days":15,"day_type":"calendar","follow_ups":[{"day":13,"action":"call_and_reply_all"}],"breach":"Dismissal"},
    {"name":"RFI_RESPONSE","label":"RFI / additional documentation","basis":"rfi_sent_at","days":15,"day_type":"calendar","follow_ups":[{"day":15,"action":"timeframe_lapsed_email_and_call"}],"breach":"Case closed"},
    {"name":"PLAN_RESPONSE","label":"Health Plan response","basis":"plan_notified_at","days":15,"day_type":"calendar","follow_ups":[{"day":13,"action":"medicaid_ahca_notify"},{"day":14,"action":"call_and_email"},{"day":15,"action":"call_again"}],"breach":"Default determination for provider"},
    {"name":"REFUND_WINDOW","label":"Complete packet after payment","basis":"outreach_at","days":7,"day_type":"calendar","breach":"Close and refund"},
    {"name":"WITHDRAWAL_LETTER","label":"Withdrawal letter","basis":"withdrawal_requested_at","days":1,"day_type":"business","breach":"Target missed"},
    {"name":"FILING_ELIGIBILITY","label":"Filing window from final determination","basis":"final_determination_at","days":365,"day_type":"calendar","breach":"Ineligible — no extensions"}
  ],
  "eligibility": {
    "thresholds": [
      {"provider_type":"hospital_inpatient","contracted":true,"min_cents":2500000},
      {"provider_type":"hospital_inpatient","contracted":false,"min_cents":1000000},
      {"provider_type":"hospital_outpatient","contracted":true,"min_cents":1000000},
      {"provider_type":"hospital_outpatient","contracted":false,"min_cents":300000},
      {"provider_type":"physician_dentist","min_cents":50000},
      {"provider_type":"rural_hospital","min_cents":0},
      {"provider_type":"other","min_cents":0}
    ],
    "filing_window_months": 12,
    "proof_of_timeliness": ["last_eob_date","service_date","appeal_documents"],
    "ineligibility_reasons": ["late_payment_only","interest_only","medicare_grievance","plan_not_fl_regulated","provider_not_fl_licensed","medicaid_fair_hearing","pending_court_action","over_12_months","pre_2000_binding_process","below_threshold","internal_process_not_exhausted"]
  },
  "fees": {"initial_fee_cents": 12359, "refund_window_days": 7, "invoice_due_days": null, "invoice_number_equals_case_number": true},
  -- Flexible payment policy (NG): "open" (no gates) | "payment_first" (fee
  -- settles before documents AND before conversion) | "custom" (explicit
  -- require_paid_before_docs / require_paid_before_convert). Optional
  -- exempt_filing_party_types bypass the gates. Runtime-editable via
  -- PUT /v1/tenants/{tenant}/program/payment-policy (platform admins).
  "payment": {"mode": "payment_first", "exempt_filing_party_types": []},
  "escalation": {"amount_trigger_cents": 100000000, "route_role": "PM", "reasons": ["fraud_waste_abuse","over_1m"]},
  "notes_streams": ["internal","coder","clinical","legal","external_agency"],
  "correspondence": {
    "agency_recipients": [],
    "templates": [
      {"key":"submission_instructions","subject":"Florida Claims Dispute Submission Process: FL AHCA","to":["requester"],"cc":[]},
      {"key":"estimate_cost","subject":"Full Review Estimate {case_number}: FL AHCA","to":["filing_party"],"cc":["agency"]},
      {"key":"estimate_followup","subject":"URGENT: Response Timeframe Lapsed — {case_number}","to":["filing_party"],"cc":["agency"],"thread":true},
      {"key":"acceptance","subject":"Results of Preliminary Review {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"ineligible","subject":"Results of Preliminary Review {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"dismissal","subject":"Dismissal {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"],"qa_role":"ATTORNEY"},
      {"key":"withdrawal","subject":"Withdrawal Request {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"plan_notification","subject":"Notification of Claims Dispute {case_number}: FL AHCA","to":["health_plan"],"cc":["agency","provider"]},
      {"key":"plan_opt_out","subject":"Plan Opt-out {case_number}: FL AHCA","to":["provider"],"cc":["agency","health_plan"]},
      {"key":"final_order","subject":"Full Review Complete {case_number}: FL AHCA","to":["agency"],"cc":["pm"]},
      {"key":"final_order_rationale_copy","subject":"Copy of Final Order Rationale {case_number}","to":["all_parties"],"cc":["agency"]},
      {"key":"rfi","subject":"Request for Additional Documentation: FL AHCA {case_number}","to":["provider_or_plan"],"cc":[]},
      {"key":"refund","subject":"FL CDR Case {case_number} Closed-Refunded","to":["filing_party"],"cc":[]},
      {"key":"payment_reminder","subject":"{case_number} Invoice - Payment Due Reminder","to":["billed_party"],"cc":[]},
      {"key":"past_due_reminder","subject":"{case_number} Past Due Payment Follow up","to":["billed_party"],"cc":[]},
      {"key":"plan_docs_notice","subject":"Additional Documentation from Health Plan – {case_number}","to":["provider"],"cc":[]}
    ]
  },
  "letter_templates": [
    {"key":"acceptance_letter","subject":"Results of Preliminary Review {case_number}","filename":"Acceptance Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"ineligible_letter","subject":"Results of Preliminary Review {case_number}","filename":"Ineligible Dispute Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"dismissal_letter","subject":"Dismissal {case_number}","filename":"Dismissal Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"withdrawal_letter","subject":"Withdrawal Request {case_number}","filename":"Withdrawal Letter - Provider {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"plan_notification_letter","subject":"Notification of Claims Dispute {case_number}","filename":"Health Plan Notification {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"plan_optout_letter","subject":"Plan Opt-out {case_number}","filename":"Plan Opt-out Letter {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"rfi_letter","subject":"Request for Additional Documentation {case_number}","filename":"Request for Additional Information {case_number}.pdf","folder":"CORRESPONDENCE"},
    {"key":"cover_letter_final_notice","subject":"Full Review Complete {case_number}","filename":"Cover Letter {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"final_order_rationale","subject":"Full Review Complete {case_number}","filename":"Review Rationale and Letter {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"default_determination","subject":"Default Determination {case_number}","filename":"Default Determination {case_number}.pdf","folder":"DETERMINATION"},
    {"key":"fl_plan_packet","subject":"Health Plan Response Form {case_number}","filename":"FL Plan Packet {case_number}.pdf","folder":"CORRESPONDENCE"}
  ],
  "deliverables": [
    {"name":"Weekly report","contract_ref":"2.4.2","due_rule":"weekly:MONDAY"},
    {"name":"Monthly report","contract_ref":"2.4.3","due_rule":"monthly:10"},
    {"name":"Ad hoc report","contract_ref":"2.4.4","due_rule":"on_request:10bd"},
    {"name":"Fee schedule / cost estimate","contract_ref":"2.3.2","due_rule":"case:INITIAL_REVIEW"},
    {"name":"Rationale per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Invoice per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Cover letter per determination","contract_ref":"2.3.3","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Case acceptance letter","contract_ref":"2.3.4","due_rule":"case:INITIAL_REVIEW"},
    {"name":"Closure letter (withdrawal/opt-out/dismissal)","contract_ref":"2.3.5","due_rule":"case:AGENCY_RECOMMENDATION"},
    {"name":"Policies & procedures: criminal background screening","contract_ref":"K.7","due_rule":"contract:execution+30d"},
    {"name":"Disaster recovery plan","contract_ref":"11","due_rule":"contract:effective-30d"},
    {"name":"Emergency operations plan","contract_ref":"2.2.7","due_rule":"contract:standing"},
    {"name":"Transition: contract documentation","contract_ref":"8.2","due_rule":"contract:end+60d"},
    {"name":"Transition cooperation agreement","contract_ref":"8.3","due_rule":"contract:end"},
    {"name":"Final report","contract_ref":"2.4.5","due_rule":"contract:end+30bd"}
  ],
  "field_schema": {
    "line_of_business": ["Medicaid","Commercial","Medicare","Medicare Advantage","Marketplace","Other"],
    "disputed_issue": ["Medicaid Medical Necessity","Underpayment","Overpayment","Denial","Other"],
    "out_of_network": ["Yes","No"],
    "case_outcome": ["TBD - case in process","Withdrawn","Dismissed","Provider Default Award","Provider Full Award","Provider Partial Award","Provider No Award","Other"],
    "party_billed": ["Health Plan","Provider","Both Parties","N/A"],
    "withdrawal_dismissed_reason": ["Dismissed-Timeliness eligibility failed","Member plan is not regulated by Florida","Self-Funded Plan","Provider No Response","Withdrawal-Claim Resolved","Other","N/A"],
    "internal_status_terminal": ["Plan Opt-Out","Ineligible","Dismissed","Withdrawn"],
    "internal_status_completed": "Decided - Invoice Paid",
    "internal_status_after_plan_notification": "Plan Notification Packet Issued",
    "_status_note": "AHCA 2026: a case is only ever CLOSED via Plan Opt-Out, Ineligible, Dismissed, or Withdrawn. A completed case is NOT closed — it rests at 'Decided - Invoice Paid'. The 60-day Agency clock never pauses (holds/RFIs/estimate window included). The 10-day initial review runs from complete-packet receipt. RFI may ride with the acceptance letter, but documentation not received by day 13 => Ineligible."
  },
  "volume_rules": {
    "comment": "Capitol Bridge Large Volume Claims Dispute Submission Policy v01.01.2026 — ADOPTED by AHCA. Governs claims-per-dispute volume (NOT repeat filer volume — AHCA confirmed 2026). Applies to disputes with >=100 claims. Non-compliant disputes are found INELIGIBLE; resubmission permitted once the ineligibility reason is cured. Written exemptions by Capitol Bridge only.",
    "source_document": "Capitol Bridge Large Volume Claims Disputes Policy 1.1.2026",
    "enabled": true,
    "large_volume_threshold_claims": 100,
    "single_cpt_per_dispute": true,
    "caps": {
      "no_medical_review": {"max_claims_per_dispute": 500, "max_claims_per_rolling_14_days": 500},
      "medical_review":    {"max_claims_per_dispute": 100, "max_claims_per_rolling_14_days": 100}
    },
    "claim_listing": {
      "format": "xlsx",
      "required_columns": ["claim_number","patient_first_name","patient_last_name","type_of_service","denial_reason","date_of_service","amount_billed","amount_paid","amount_in_dispute","date_claim_submitted","date_of_denial","date_of_final_determination","provider_name","facility_name","evidence_location"],
      "claim_numbers_must_match_eobs": true
    },
    "documentation": {
      "searchable_required": true,
      "filenames_must_match_contents": true,
      "combined_pdf_categories": ["EOBs","Appeal documents","Medical documentation"],
      "unlocked_unrestricted_required": true,
      "disallowed_file_types": ["EDIDATA","BAK"]
    },
    "exemptions": "effective only with prior written approval from Capitol Bridge (flcdr@capitolbridge.com)",
    "noncompliance": {"disposition": "INELIGIBLE", "resubmission_allowed": true}
  },
  "rules": [
    {
      "name": "intake-day13-incomplete",
      "event": "sweep.intake",
      "enabled": true,
      "_basis": "AHCA 2026: RFI may ride with the acceptance letter, but if documentation is not received by the 13th day the case is found incomplete and an ineligibility letter issues",
      "conditions": [
        {"field": "days_since_outreach", "op": "gte", "value": 13},
        {"field": "status", "op": "in", "value": ["INSTRUCTED", "DOCS_RECEIVED"]}
      ],
      "actions": [
        {"type": "set_status", "params": {"status": "INELIGIBLE"}},
        {"type": "notify", "params": {"kind": "SLA_BREACH", "body": "Intake {{id}} ({{email}}) found incomplete at day {{days}} — issue ineligibility letter"}}
      ]
    },
    {
      "name": "doc-unverified-fields-review",
      "event": "doc.analyzed",
      "enabled": true,
      "_basis": "Doc-intel grounding check: extracted fields that cannot be traced to source text must be human-verified before they feed a determination",
      "conditions": [
        {"field": "ungrounded_count", "op": "gt", "value": 0}
      ],
      "actions": [
        {"type": "flag_review", "params": {"reason": "{{ungrounded_count}} unverified field(s) extracted from {{doc_type}} — requires human confirmation"}},
        {"type": "notify", "params": {"kind": "MILESTONE", "body": "Document {{doc_id}} ({{doc_type}}) analyzed with {{ungrounded_count}} unverified field(s) — flagged for review"}}
      ]
    },
    {
      "name": "doc-poor-scan-review",
      "event": "doc.analyzed",
      "enabled": true,
      "_basis": "Barely-legible scans are valid evidence but extraction confidence is degraded; staff should sight the original before relying on extracted values",
      "conditions": [
        {"field": "scan_quality_poor", "op": "eq", "value": true}
      ],
      "actions": [
        {"type": "flag_review", "params": {"reason": "Poor scan quality on {{doc_id}} — verify extracted values against the original"}}
      ]
    }
  ]
}
$$::jsonb)
ON CONFLICT (tenant) DO UPDATE SET config=EXCLUDED.config, program=EXCLUDED.program, updated_at=now();

-- ─────────────────────────────────────────────────────────────────────
-- Program-config protection (2026): fees, billing, and reconciliation
-- subtrees are MIGRATION-ONLY. The API's DB role can update {rules} and
-- {manifest} at runtime, but any change to config->'fees', ->'billing', or
-- ->'reconciliation' is rejected unless the session explicitly opts in:
--     SET LOCAL app.config_migration = 'on';   -- migrations only
-- Every config change (guarded or not) is written to
-- program_config_history — the tamper-evident trail for direct-SQL edits
-- that bypass the application's own audit path.
-- ─────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS public.program_config_history (
  id         bigserial PRIMARY KEY,
  tenant     text NOT NULL,
  changed_by text NOT NULL DEFAULT current_user,
  changed_at timestamptz NOT NULL DEFAULT now(),
  old_config jsonb,
  new_config jsonb
);

CREATE OR REPLACE FUNCTION public.guard_program_config() RETURNS trigger AS $$
BEGIN
  IF current_setting('app.config_migration', true) IS DISTINCT FROM 'on' AND (
       NEW.config->'fees'           IS DISTINCT FROM OLD.config->'fees'
    OR NEW.config->'billing'        IS DISTINCT FROM OLD.config->'billing'
    OR NEW.config->'reconciliation' IS DISTINCT FROM OLD.config->'reconciliation') THEN
    RAISE EXCEPTION 'program fees/billing/reconciliation are migration-only: SET LOCAL app.config_migration = ''on'' inside a migration transaction';
  END IF;
  IF NEW.config IS DISTINCT FROM OLD.config THEN
    INSERT INTO public.program_config_history (tenant, old_config, new_config)
    VALUES (OLD.tenant, OLD.config, NEW.config);
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS program_config_guard ON public.program_rules;
CREATE TRIGGER program_config_guard BEFORE UPDATE ON public.program_rules
  FOR EACH ROW EXECUTE FUNCTION public.guard_program_config();
