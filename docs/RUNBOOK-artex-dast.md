<!-- Author: Patrick Munis at newwave -->
# Runbook: ARTEX Autonomous DAST Against the IDRE Platform

ARTEX (https://github.com/BaaRai/ARTEX, AGPL-3.0) is our dynamic security
testing tool: an LLM planner decomposes an attack goal, workers execute with
real tools, and every finding stays bound to the captured HTTP request and
response that prove it. It fills the gap the static CI (`security.yml`:
gosec, govulncheck, trivy, bandit) cannot — how the platform behaves against
a live adversary.

## Hard rules (read before anything else)

1. **Disposable stack only.** ARTEX runs against the local compose stack
   seeded with test data. Never staging, never production, never any host
   with real tenant data. An autonomous agent WILL mutate what it can reach.
2. **Scope is law.** Every task gets the allow-list in
   `deploy/artex/task-pack.md` (localhost + compose service names). If a
   task ever shows activity toward a non-local target, stop it immediately
   and treat it as an incident.
3. **Test identities are weak by design** (`seed-artex-test-users.sh`).
   They exist so the scanner can authenticate per role. They must never
   exist outside the disposable stack — the k8s realm is operator-managed
   and never imports them.
4. **Approval gates stay on.** ARTEX pauses for human approval on
   intercept-rule matches (bulk exports, destructive verbs). Do not
   blanket-approve.
5. **ARTEX is not part of the platform.** AGPL-3.0, internal tool. It never
   ships in a deployment and never touches the platform's database — it has
   its own (`artex-db`).

## Setup (one time, ~15 min)

```bash
# 1. Platform stack up and seeded
docker compose up -d
psql "$DATABASE_URL" -f scripts/seed-honeytokens.sql   # if not already seeded

# 2. Test identities
bash scripts/seed-artex-test-users.sh

# 3. Ollama model for the ARTEX planner (strongest the box can hold;
#    planner quality = coverage quality)
docker exec $(docker compose ps -q ollama) ollama pull qwen2.5:3b   # minimum

# 4. ARTEX overlay (same compose project => joins the platform network)
docker compose -f docker-compose.yml -f deploy/artex/docker-compose.artex.yml up -d
docker compose logs artex | grep "Setup token"   # one-time UI token
```

Open http://localhost:8787 → `/setup` with the token, then:

- **System → LLM:** add a profile pointing at the Ollama endpoint
  (`http://ollama:11434`; the overlay pre-fills this and permits the
  private-endpoint model fetch). No API keys anywhere — local only.
- **System → Settings:** concurrent workers ≥ 2.
- Verify the profile on a trivial task before the real pack.

## Running the pack

Work through `deploy/artex/task-pack.md` in order — **T1 (cross-tenant)
always first**: it tests the control everything else depends on, and a
failure there stops the exercise.

Per task: paste the goal verbatim, attach the listed test assets, apply the
scope allow-list, run. Watch two surfaces while it works:

1. **ARTEX findings** — each carries the proving request/response. Triage
   is: reproduce the captured request yourself → confirm → file it.
2. **Wazuh alerts** — the task pack names the rule ID that must fire for
   detection-sensitive tasks (T1, T4, T5, T7).

## The detection scorecard (the second deliverable)

ARTEX tells you what an attacker CAN do; Wazuh tells you whether you'd SEE
it. After each task, record one of three outcomes per the scorecard table
in the task pack. A "blocked + silent" row is a detection gap: write or
adjust the Wazuh rule (in `deploy/wazuh/rules/idre-rules.xml`) until the
attempt is visible. Re-run the task to confirm.

## Findings → fixes → regression

1. Every confirmed finding gets a fix in the next implementation wave,
   committed with the ARTEX evidence (request/response) quoted in the
   commit message or linked issue.
2. After the fix ships, use ARTEX **Retest** on the finding — retest
   re-runs the proving traffic. A passed retest closes the finding and
   becomes the DAST regression for that hole.
3. Dev-mode artifacts (Permify bypass with `PERMIFY_URL` unset, dev token
   defaults, MinIO fallback) are triaged separately: they get either
   fail-fast startup checks in prod profile or a documented acceptance —
   not silence.

## Cadence

- **Full pack:** before every production deploy window and after any wave
  that touches authn, tenancy, payments, or the vault/storage boundary.
- **T1+T4+T5 only:** after any wave touching middleware or internal tokens
  (30-minute smoke).
- **Nightly (optional):** CI job that spins up the compose stack, runs T1,
  T2, T4, T9 headlessly via the ARTEX task API, and fails on new
  critical/high findings. Keep it out of the PR path — local-model
  inference is too slow for per-PR gating.

## Teardown

```bash
docker compose -f docker-compose.yml -f deploy/artex/docker-compose.artex.yml down
# Findings/evidence persist in the artex-data volume; wipe for a clean cycle:
docker volume rm $(docker volume ls -q | grep artex)
```
