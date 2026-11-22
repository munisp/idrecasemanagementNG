<!-- Author: Patrick Munis at newwave -->
# Runbook: Incident Communications

Availability incidents on this platform affect 50 state IDRE entities and,
through them, patients waiting on dispute resolutions. During the 2026-11
outage week the worst damage was not the downtime — it was organizations
discovering outages from their own users because comms lagged by hours.
This runbook makes comms a timed, owned, practiced activity, not an
afterthought.

## Severity matrix

| Sev | Definition | Examples | Page? |
|-----|-----------|----------|-------|
| S1 | Platform unusable or data at risk | case-api down, Postgres primary lost, audit-chain mismatch, confirmed breach | Yes, immediately |
| S2 | Major function degraded, workaround exists | Stripe/SMTP breaker open, outbox relay stalled, one tenant's intake failing | Yes |
| S3 | Minor degradation | elevated p95 latency, one worker type lagging | No — ticket + watch |

An open vendor breaker is **S2, not "their problem"**: the platform is
degrading gracefully by design, but tenants still need to know payments or
emails are queued.

## Clock (S1/S2)

| T+ | Action | Owner |
|----|--------|-------|
| 0–5 min | Acknowledge page. Declare severity. Open incident channel `#idre-inc-YYYYMMDD-<n>` | On-call engineer (now IC: Incident Commander) |
| 5–15 min | First internal update in channel: what we know, what we don't, next update time. Start the timeline log | IC |
| 15 min | Status page: post "Investigating" (template below). If S1, also email affected state tenant admins | IC or comms deputy |
| 15–30 min | Second internal update even if nothing new — "still investigating, next update at HH:MM" | IC |
| Every 30 min | Internal update. Status page update every 60 min minimum, or immediately on material change | IC |
| Mitigation applied | Status page: "Monitoring" — state what was done, what we're watching | IC |
| +60 min stable | Status page: "Resolved" + tenant follow-up email with summary | IC |
| 5 business days | Postmortem published (blameless, timeline, 5-whys, action items with owners and dates) | IC + engineering lead |

## Status page templates

**Investigating**
> We are investigating reports of [symptom, e.g. "failed case submissions"].
> Affected: [all tenants | tenant(s): XX, YY]. Next update by [HH:MM TZ].

**Identified / Monitoring**
> We have identified the cause as [one plain sentence — no internal jargon,
> no vendor-blaming]. We have [mitigation applied] and are monitoring.
> Next update by [HH:MM TZ].

**Resolved**
> Service is fully restored as of [HH:MM TZ]. Impact window: [start–end].
> [If applicable: No case data was lost; N queued [payments|emails|events]
> are being delivered automatically.] A postmortem will follow within 5
> business days.

## Vendor outage comms (breaker open)

When `ExternalBreakerOpen` fires, the status page wording matters — say what
the user experiences, not the internals:

> Payment processing is temporarily unavailable due to an outage at our
> payment processor. Submitted determinations are safe and queued; payments
> will process automatically when service recovers. No action needed.

Never name-drop blame beyond "our payment processor/email provider" in the
first 2 hours — early attribution is usually wrong (the outage week proved
even the vendors didn't know their own root cause for hours).

## Who gets told what

| Audience | Channel | When |
|----------|---------|------|
| Internal engineering | `#idre-inc-*` channel | Continuous |
| State tenant admins (affected) | Email + status page | S1: T+15min; S2: within 60 min |
| All tenants | Status page only | S2+ |
| Federal partners (if reporting windows affected) | Direct email from platform lead | S1 during a reporting/freeze window (see docs/CHANGE-FREEZE.md) |
| Patients/providers (public) | Nothing direct — tenants own their user comms | Never |

## Rules that prevent the classic failures

1. **The IC owns comms, not the person fixing.** If the IC is also the only
   person who can fix it, appoint a comms deputy first.
2. **Updates go out on the clock even when content is "no change."** Silence
   reads as "nobody is working on it."
3. **Say what you know, flag what you don't.** "Cause unknown, scope is all
   tenants, next update 14:30" is a good update.
4. **Never promise a fix time you don't control.** Especially with vendor
   outages.
5. **Retract loudly.** If a status post was wrong, correct it in a new
   update, not an edit.
6. **Log everything in the timeline** as it happens — the postmortem is
   assembled from the log, not from memory.
