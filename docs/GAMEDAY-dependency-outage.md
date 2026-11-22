<!-- Author: Patrick Munis at newwave -->
# Game Day: Dependency-Outage Drill

Quarterly, first Tuesday of Jan/Apr/Jul/Oct, 10:00 local, 90 minutes, on
**staging** (never production). Purpose: prove that when a dependency dies —
vendor, broker, or our own infrastructure — the platform degrades the way
the design says it does, and that humans execute the comms runbook. The
2026-11 outage week showed real dependencies fail without warning; the only
organizations that looked good were the ones that had rehearsed.

Participants: on-call engineer (plays IC), one engineer injecting failures,
one observer scoring. Nobody gets advance notice of *which* scenario runs.

## Scenarios (rotate, one per drill; scenario 1 every time)

### 1. Payment processor down (mandatory)
**Inject:** compose — `docker compose stop stripe-mock` (or block egress to
the processor endpoint via the staging firewall).
**Expected:**
- Within ~3 failed calls, the `stripe` breaker opens;
  `idre_breaker_state{target="stripe"}` goes to 2.
- New payment submissions return 503-with-retry guidance; **no 500s, no
  hung requests, no lost determinations** — work is queued.
- `ExternalBreakerOpen` pages within 2 minutes.
- IC posts "Investigating" to the status page within 15 minutes using the
  vendor-outage wording (processor unnamed in first 2h).
**Recover:** restart the dependency; half-open probe closes the breaker;
queued payments drain without manual replay.
**Pass:** all of the above, and the timeline log is complete.

### 2. Email provider down
**Inject:** block SMTP egress (staging firewall rule) or point `SMTP_HOST`
at a blackhole address and restart case-api.
**Expected:** `smtp` breaker opens after 5 failures; case workflow
notifications queue in the outbox rather than failing the parent
transaction; users see no errors on unrelated actions; comms per runbook.
**Recover:** queued mail delivers automatically after recovery.

### 3. Kafka down
**Inject:** `docker compose stop kafka`.
**Expected:** writes still succeed (transactional outbox is in Postgres);
`idre_outbox_unpublished` climbs; `OutboxBacklogGrowing` fires at >1000;
KEDA keeps relay replicas warm; downstream workers (doc-intel, lettergen,
analytics) stall but nothing errors.
**Recover:** relay drains the backlog; verify `scripts/verify-audit-chain.py`
still passes and zero events were lost (row count before/after matches).

### 4. Postgres primary failover
**Inject:** `kubectl cnpg failover` (k8s) or `docker compose stop postgres`
(compose, single-node: this is the full-down case).
**Expected:** k8s — CNPG promotes a replica, pgbouncer re-routes, brief
write errors only; compose — clean 503s at the edge, no half-written state
(transactional outbox means no partial events).
**Recover:** full write path restored; audit chain verified.

### 5. MinIO / storage gateway down
**Inject:** `docker compose stop storage` (OpenDAL gateway).
**Expected:** document uploads fail fast with a clear 503 (fail-closed by
design — no local plaintext spillover, no silent fallback); case metadata
operations continue; alert fires on gateway health.
**Recover:** uploads resume; previously submitted ciphertext objects intact.

## Scorecard (observer fills during the drill)

| Criterion | Pass? |
|-----------|-------|
| Degradation matched design (breaker/queue/fail-closed as specified) | |
| No data loss or corruption (counts + audit chain verified after) | |
| Alert fired within its `for:` window | |
| Status page "Investigating" within 15 min | |
| Correct template used (vendor unnamed early, user-impact wording) | |
| Timeline log complete enough to write a postmortem from | |
| Recovery automatic (no manual data repair) | |
| Drill finished with a written retro and at most 3 action items | |

## Rules

- **One scenario at a time.** Combined failures come after the singles all
  pass.
- **Timebox injection to 20 minutes** even if the team wants to keep
  poking — recovery practice matters as much as failure practice.
- **Every surprise is an action item** with an owner and a date, filed
  before the drill ends. Surprises are the deliverable; a drill where
  nothing surprised anyone wasn't hard enough.
- **After each drill, update this doc** with anything the platform did that
  isn't written down here.
