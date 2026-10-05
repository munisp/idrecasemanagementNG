-- Program Manifest seeds — the sector-agnostic lifecycle/terminology layer.
-- Apply AFTER program-rules.sql (needs public.program_rules rows to exist).
-- Tenants without a manifest behave exactly as before (legacy fallbacks).

-- 1. FL AHCA Claims Dispute Resolution — formalizes today's live behavior
--    as a manifest (no behavior change; proves the format on the anchor tenant).
UPDATE public.program_rules
SET config = jsonb_set(config, '{manifest}', $$
{
  "program": "fl-ahca-claims-dispute",
  "version": "1.0.2026",
  "sector": "healthcare",
  "terminology": {
    "case_noun": "Dispute", "case_plural": "Disputes",
    "party_a": "Provider", "party_b": "Health Plan",
    "neutral": "Reviewer", "intake_noun": "Intake Request"
  },
  "lifecycle": {
    "intake_statuses": [
      {"name": "INSTRUCTED", "label": "Packet requested"},
      {"name": "DOCS_RECEIVED", "label": "Docs received"},
      {"name": "PACKET_COMPLETE", "label": "Packet complete", "anchors_clock": "packet_complete_at"},
      {"name": "PAID", "label": "Fee paid"},
      {"name": "CONVERTED", "label": "Converted to dispute", "terminal": true},
      {"name": "INELIGIBLE", "label": "Ineligible", "terminal": true},
      {"name": "CLOSED_REFUNDED", "label": "Closed (refunded)", "terminal": true}
    ],
    "case_statuses": [
      {"name": "ACCEPTED", "label": "Accepted"},
      {"name": "PLAN_NOTIFICATION", "label": "Plan Notification Packet Issued"},
      {"name": "IN_REVIEW", "label": "In review"},
      {"name": "DECIDED", "label": "Decided - Invoice Paid", "completed": true},
      {"name": "PLAN_OPT_OUT", "label": "Plan Opt-Out", "terminal": true},
      {"name": "INELIGIBLE", "label": "Ineligible", "terminal": true},
      {"name": "DISMISSED", "label": "Dismissed", "terminal": true},
      {"name": "WITHDRAWN", "label": "Withdrawn", "terminal": true}
    ]
  },
  "clocks": [
    {"name": "initial_review", "days": 10, "day_type": "calendar", "basis": "packet_complete_at"},
    {"name": "agency_determination", "days": 60, "day_type": "calendar", "basis": "initiation_at"},
    {"name": "completeness_gate", "days": 13, "day_type": "calendar", "basis": "outreach_at"}
  ]
}
$$::jsonb, true)
WHERE tenant = 'fl';

-- 2. TX auto appraisal (SB 458, effective 2026-01-01) — the first non-
--    healthcare sector, installed as config. Proves the Swiss-army-knife
--    claim: no code changed for this tenant.
INSERT INTO public.program_rules (tenant, program, config)
VALUES ('tx', 'tx-auto-appraisal', $$
{
  "manifest": {
    "program": "tx-auto-appraisal",
    "version": "1.0.2026",
    "sector": "insurance-appraisal",
    "terminology": {
      "case_noun": "Appraisal", "case_plural": "Appraisals",
      "party_a": "Policyholder", "party_b": "Insurer",
      "neutral": "Umpire", "intake_noun": "Demand"
    },
    "lifecycle": {
      "intake_statuses": [
        {"name": "DEMAND_RECEIVED", "label": "Demand received"},
        {"name": "DOCS_RECEIVED", "label": "Evidence received"},
        {"name": "PACKET_COMPLETE", "label": "Demand complete", "anchors_clock": "packet_complete_at"},
        {"name": "CONVERTED", "label": "Appraisal opened", "terminal": true},
        {"name": "INELIGIBLE", "label": "Ineligible", "terminal": true},
        {"name": "CLOSED_REFUNDED", "label": "Closed (refunded)", "terminal": true}
      ],
      "case_statuses": [
        {"name": "APPRAISERS_APPOINTED", "label": "Appraisers appointed"},
        {"name": "NEGOTIATING", "label": "Appraisers negotiating"},
        {"name": "UMPIRE_ASSIGNED", "label": "Umpire assigned"},
        {"name": "AWARDED", "label": "Award signed"},
        {"name": "PAID", "label": "Award paid", "terminal": true, "completed": true},
        {"name": "DISMISSED", "label": "Dismissed", "terminal": true},
        {"name": "WITHDRAWN", "label": "Withdrawn", "terminal": true}
      ]
    },
    "clocks": [
      {"name": "demand_window", "days": 120, "day_type": "calendar", "basis": "loss_notice_at"},
      {"name": "negotiation_window", "days": 75, "day_type": "calendar", "basis": "demand_at"},
      {"name": "umpire_award", "days": 180, "day_type": "calendar", "basis": "umpire_assigned_at"}
    ]
  },
  "rules": [
    {
      "name": "umpire-deadline-breach",
      "event": "sweep.case",
      "enabled": true,
      "_basis": "TX SB 458: property appraisal umpire award due within 180 days of umpire selection",
      "conditions": [
        {"field": "status", "op": "eq", "value": "UMPIRE_ASSIGNED"},
        {"field": "days_since_assignment", "op": "gte", "value": 170}
      ],
      "actions": [
        {"type": "notify", "params": {"kind": "SLA_BREACH", "body": "Appraisal {{case_id}}: umpire award due in 10 days — escalate"}}
      ]
    }
  ]
}
$$::jsonb)
ON CONFLICT (tenant) DO UPDATE SET config=EXCLUDED.config, program=EXCLUDED.program, updated_at=now();
