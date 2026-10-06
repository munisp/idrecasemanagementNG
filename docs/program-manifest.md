# Program Manifest — the sector-agnostic keystone

A **Program Manifest** is a versioned JSON document that declares everything
the platform needs to run one dispute-resolution program (a sector ×
jurisdiction pair: "FL AHCA claims dispute", "Federal NSA IDR", "TX auto
appraisal"). Manifests live in DATA — `public.program_rules.config.manifest`
— are loaded fresh per request, and are edited through the admin API with the
same append-only audit trail as rules.

**Onboarding a sector = installing a manifest, not writing code.**

## Format

```jsonc
{
  "program": "tx-auto-appraisal",
  "version": "1.0.2026",
  "sector": "insurance-appraisal",

  // Display vocabulary. Every user-facing noun in the portal renders from
  // here — "Case"/"Provider"/"Health Plan" never appear in code.
  "terminology": {
    "case_noun":   "Appraisal",        // singular, e.g. "Case", "Appeal"
    "case_plural": "Appraisals",
    "party_a":     "Policyholder",     // filing party (usually)
    "party_b":     "Insurer",          // responding party
    "neutral":     "Umpire",           // the secondary arbitrator
    "intake_noun": "Demand"
  },

  // Lifecycle state machine. The ONLY source of truth for valid statuses,
  // transitions, and terminal states — Go and the portal both read this.
  "lifecycle": {
    "intake_statuses": [
      { "name": "INSTRUCTED",      "label": "Packet requested" },
      { "name": "DOCS_RECEIVED",   "label": "Docs received" },
      { "name": "PACKET_COMPLETE", "label": "Packet complete", "anchors_clock": "packet_complete_at" },
      { "name": "CONVERTED",       "label": "Converted", "terminal": true },
      { "name": "INELIGIBLE",      "label": "Ineligible", "terminal": true },
      { "name": "CLOSED_REFUNDED", "label": "Closed (refunded)", "terminal": true }
    ],
    "case_statuses": [
      { "name": "DEMANDED",    "label": "Demanded" },
      { "name": "NEGOTIATING", "label": "Appraisers negotiating" },
      { "name": "UMPIRE_ASSIGNED", "label": "Umpire assigned" },
      { "name": "AWARDED",     "label": "Awarded" },
      { "name": "PAID",        "label": "Paid", "terminal": true, "completed": true }
    ]
  },

  // Statutory clocks: name, length, day-counting rule, anchor fact.
  "clocks": [
    { "name": "negotiation_window", "days": 75, "day_type": "calendar",
      "basis": "demand_at", "on_expiry": { "event": "sweep.negotiation" } }
  ],

  // Business policy — same rule engine as before (events, conditions,
  // actions); bundled in the manifest so a sector pack is one document.
  "rules": []
}
```

## Semantics

- **Fresh reads**: every request loads the manifest from Postgres — an admin
  edit takes effect immediately, no restart.
- **Validation on write**: `PUT /manifest` rejects manifests with duplicate
  status names, clocks anchored to unknown fields, unknown rule events/ops,
  or missing required terminology — with per-item errors. Nothing invalid
  ever becomes live policy.
- **Audit**: every write appends before/after to `public.rule_changes`
  (shared append-only table; `note` distinguishes rule vs manifest edits).
- **Backwards compatibility**: tenants without a manifest behave exactly as
  today — the Go side falls back to the built-in FL/NSA status set, the
  portal to its default pipeline. Manifests are opt-in per tenant.
- **fail-closed**: an invalid stored manifest (edited out-of-band) behaves as
  "no manifest" for labels but blocks status transitions rather than guessing.

## Workflow clocks (Temporal)

The case lifecycle workflow (`IdrCaseWorkflow`) resolves every phase timeout
from `clocks` — loaded once per case via an activity (recorded in workflow
history, so replay is deterministic; new cases pick up edits immediately).
Reserved names:

| clock name             | phase                                   | NSA default    |
|------------------------|------------------------------------------|----------------|
| `response_window`      | non-initiating party response            | 3 business     |
| `offer_window`         | double-blind offer submission            | 10 business    |
| `selection_window`     | neutral selection                        | 10 calendar    |
| `determination_window` | neutral's written determination          | 30 business    |
| `payment_window`       | non-prevailing party pays                | 30 calendar    |

`day_type: "business"` skips weekends and the tenant's `public.holidays`
rows; `"calendar"` is plain addition. Any name absent from the manifest keeps
its NSA default, so legacy tenants behave identically. Breach escalations are
named after the actual clock (e.g. `DETERMINATION_75CD`).

Two feature flags change the workflow's shape, not just the UI:

- `features.negotiation_window: false` — the workflow skips the negotiation
  pre-phase entirely and opens the case at initiation.
- `features.sealed_offers: false` — the offer-window and vault-reveal phases
  are skipped; the case moves from response to neutral selection (e.g. TX
  auto appraisal, where the umpire weighs appraisals rather than picking a
  sealed number).

## API

| Endpoint | Role | Purpose |
|---|---|---|
| `GET  /v1/tenants/{t}/manifest` | any authenticated | current manifest (portal renders from this) |
| `PUT  /v1/tenants/{t}/manifest` | FEDERAL_ADMIN / PLATFORM_ADMIN (+ Permify `program_rules.edit`) | validate + replace + audit |

## Sector packs (roadmap)

A sector pack is a versioned archive containing the manifest, doc-intel
extraction schemas, letter templates, and reference data — installable via
the admin UI. The manifest format above is the pack's root document.
