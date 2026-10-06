# Runbooks

## 1. Ledger reconciliation drift (`LEDGER_RECONCILIATION` breach)

**Fired by:** daily `LedgerReconciliationWorkflow`; row in
`public.ledger_reconciliation` with `status='DRIFT'` + `sla_breaches` row.

**Meaning:** Postgres payments and TigerBeetle balances disagree.

1. Identify the check: `SELECT * FROM public.ledger_reconciliation WHERE status='DRIFT' ORDER BY ran_at DESC;`
   - `clearing_check` drift → a Stripe webhook's ledger leg failed (postPaymentLedger is fail-open by design — the money moved, the ledger didn't).
   - `ledger_invariant` drift → a fee transfer posted to the wrong account pair, or a pending transfer was neither posted nor voided (check stuck pendings).
2. Find the window: last `OK` row vs first `DRIFT` row → payments in that window:
   `SELECT * FROM public.payments WHERE tenant=$1 AND updated_at BETWEEN $ok AND $drift ORDER BY updated_at;`
3. Repair: for each missing leg, re-post with the deterministic id
   (`sha256("stripe:"+payment_intent+direction)`) — idempotent, safe to replay.
   Never hand-edit TB; only post correcting transfers.
4. Verify: re-run the reconcile activity manually (`tctl workflow start --workflow_type LedgerReconciliationWorkflow ...` with today's id suffix `-manual`), expect `OK`.
5. Postmortem: if `postPaymentLedger` logged "ledger leg failed", the root cause is TB connectivity during a webhook — check TB replica health timeline.

## 2. Outbox backlog (KEDA scaled outbox-relay to max, lag still growing)

**Symptoms:** `published_at IS NULL` rows growing; KEDA at 8 replicas.

1. Check Kafka health first — relay backlog is usually downstream:
   broker under-replicated partitions, or the relay's idempotent producer fenced.
2. Check relay logs for `producer.flush` timeouts or `postgres unavailable`.
3. If Postgres is the constraint: each replica holds a txn per poll cycle —
   8 replicas × `RELAY_BATCH=1000` rows claimed. If claims collide, *lower*
   replicas and raise `RELAY_BATCH` (lock contention beats parallelism past 8).
4. If topics are missing for a new tenant: re-run `provision-tenant.sh $st`
   (idempotent) — relay skips unknown tenants only until migrated.
5. Never delete outbox rows. If a poison payload blocks ordering, quarantine:
   mark it published with a `poison:` comment in a compensating audit row,
   and emit the event manually after fixing the payload.

## 3. Temporal workflow storm (matching backlog / timer fires spiking)

**Symptoms:** `temporal_matching` backlog gauge rising; workflow task latency > 5s.

1. Distinguish cause:
   - **Timer mass-expiry** (e.g. 1000 negotiation windows opened the same day):
     benign, drains on its own — workers scale by task queue; raise
     `max_concurrent_workflow_tasks` only if worker CPU < 60%.
   - **Activity retries looping**: check `retryTransient` exhaustion in
     `tctl workflow describe` — a downstream (vault/case-api) outage retries
     activities; fix the downstream, don't touch Temporal.
   - **Hot shard**: with 4096 shards this is rare; if one workflow id range is
     hot, it's a runaway signal loop in application code — find it via
     `tctl workflow list` filtered by status.
2. Circuit breakers (resiliency.yaml) protect vault/case-api — if they're
   tripping, that's the diagnosis, not the disease.
3. Do **not** restart the history service to "clear" backlog — it replays
   everything and makes it worse. Scale matching pods first.

## 4. Vault master-key rotation

1. Generate new key into K8s secret `vault-master` (new key version field).
2. Rolling restart vault-rs with dual-key env: old key decrypts, new key
   encrypts on write; background re-seal job touches each sealed blob once.
3. Only after `sealed_blobs` all report new key version: drop the old key.
4. **Never** lose the only copy — there is no recovery path (by design).
