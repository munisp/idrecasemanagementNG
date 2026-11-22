<!-- Author: Patrick Munis at newwave -->
# Change Freeze Calendar & Deploy Policy

The 2026-11 outage week (Microsoft 365 capacity upgrade, FAA router, JPMorgan
software bug, NSA Net failed patch) was three self-inflicted outages in five
days — none of them attacks. Every production change on this platform goes
through this policy. The rule is simple: **no changes when tenants can least
afford an outage, staged changes everywhere else.**

## Deploy train (normal weeks)

| Day | What ships | Gate |
|-----|-----------|------|
| Tuesday | case-api + workers (Argo Rollouts canary, 10/25/50/100) | SLO AnalysisTemplate passes at each step |
| Wednesday | PWA static assets, docs, dashboards | Staging smoke + one reviewer |
| Thursday | DB migrations, infra/config changes | Rehearsed on staging with production-shape data volume; rollback script attached to the change ticket |

Migrations ship on Thursdays, never Fridays — a bad migration found at 17:00
Friday becomes a weekend incident.

## Freeze windows

Hard freeze = no deploys, no migrations, no infra/config changes, no
cert/DNS/IdP changes. Monitoring and read-only operations continue.

| Window | Dates (recurring) | Why |
|--------|-------------------|-----|
| CMS Open Enrollment | Nov 1 – Jan 15 | Peak dispute volume follows enrollment; state IDRE portals are under public scrutiny |
| Federal reporting | Dec 1 – Jan 31 | Annual IDRE reporting to the Departments (HHS/Treasury/DOL); tenants are finalizing submissions |
| Holiday | Dec 22 – Jan 2 | Minimal staffing on both our side and tenant side; incident MTTR doubles |
| Plan-year rate filing | May 1 – Aug 1 (soft) | Issuer/TPA attestation season — elevated case intake; **soft freeze: Tuesdays-only deploys, no migrations** |
| Quarter close | Last 5 business days of Mar/Jun/Sep | Settlement reconciliation runs; ledger queries are heaviest |

Dates shift when CMS publishes its annual guidance; the on-call lead updates
this table each October and the freeze calendar is mirrored into the deploy
pipeline (see below).

## Freeze exception process

Only two categories may ship during a hard freeze:

1. **Emergency security patch** — actively exploited vuln or credential
   compromise. Requires: on-call lead + security lead sign-off in the ticket,
   canary deploy, and a 24h watch after.
2. **Data-loss or compliance risk** — e.g. an audit-chain defect or a
   retention bug that would put a tenant in violation of 45 CFR 149.
   Same sign-off, plus written tenant notification before the change.

Everything else waits. "The demo is Monday" is not an exception.

## Enforcement (not just policy)

- The freeze calendar lives in `deploy/freeze-windows.json` and is enforced
  by `scripts/check-freeze-window.py`, which every deploy runs through the
  `deploy-gate` workflow (`.github/workflows/deploy-gate.yml`, reusable via
  `workflow_call`). The job fails inside a hard freeze unless the exception
  flag is set, and applies soft-freeze rules (Tuesdays-only deploys, no
  migrations, CAB approval for infra) automatically.
- Argo Rollouts auto-abort (see `rollout-case-api.yaml`) provides the last
  line of defense even for permitted changes: a change that degrades the
  SLO rolls itself back without waiting for a human.
- Any freeze exception gets a written retro within 5 business days: did the
  canary catch what it should have, did comms fire, what do we change.

## Pre-freeze checklist (run the Friday before each window)

- [ ] All in-flight rollouts completed or rolled back — nothing parked at 25%
- [ ] Outbox backlog < 100 and relay healthy (no events stranded over freeze)
- [ ] Backup restore drill passed within the last 30 days
  (`docs/RUNBOOK-backup-restore.md`)
- [ ] On-call rota confirmed for the window, including tenant-side contacts
- [ ] Pending cert/secret expiries checked — nothing expires inside the window
- [ ] Status page and comms templates tested once (fire and retract a test
      incident during business hours)
