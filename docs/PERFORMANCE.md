# Performance & Capacity Guide

How the platform is tuned for high throughput, what each knob does, and an
honest capacity model for "millions of transactions per second".

## Honest capacity model

End-to-end throughput is set by the **slowest stage in the critical path**,
not by the fastest component:

| Stage | Sustained capability (tuned) | Notes |
|---|---|---|
| APISIX edge | 100k+ req/s per 3 pods | event-driven; autoscale to 12 |
| case-api (Go) | 20–50k req/s per replica | pgxpool 50 conns × replicas |
| **Postgres (writes)** | **~20–60k simple txn/s per primary** | the real ceiling for API writes |
| Kafka | 1M+ msg/s per 3 brokers | lz4 + batching; not the bottleneck |
| **TigerBeetle** | **~1M transfers/s per 6-replica cluster** | the component that *can* do it |
| Temporal | ~10–50k workflow tasks/s @ 4096 shards | size shards at bootstrap |
| Outbox relay | ~50k events/s per pod | lz4 + 10k batches + SKIP LOCKED |

So: **millions of *ledger* transactions per second is achievable inside
TigerBeetle** (that's what it's built for — batched, replicated, in-memory
with io_uring). **Millions of *API* transactions per second is not** a
Postgres-backed CRUD pattern — no relational primary does that on one writer.
The architecture's answer is the one the payments industry uses: keep the OLTP
path lean (PgxPool + PgBouncer + tuned WAL), push volume through the ledger
(batched TigerBeetle transfers, one DB row per settlement batch instead of per
ledger line), and make everything downstream async (outbox → Kafka → lakehouse).

Practical targets per state tenant: 5–10k API req/s burst, 100k+ ledger
transfers/s during fee-settlement sweeps, millions of lakehouse events/day.

## What was tuned (this pass)

### Postgres — `deploy/helm-values/postgres.yaml`
`shared_buffers` 16 GB (25%), `effective_cache_size` 48 GB, `work_mem` 64 MB,
`wal_compression=lz4`, `max_wal_size` 16 GB, `checkpoint_completion_target`
0.9, `random_page_cost` 1.1 + `effective_io_concurrency` 200 (SSD cost model),
parallel workers 16/4, autovacuum 6 workers with tighter scale factors
(schema-per-tenant means 50× the hot tables), `idle_in_transaction_session_timeout`
60 s and `statement_timeout` 30 s guardrails. `max_connections` 500 assumes the
**PgBouncer sidecar (transaction pooling)** — direct clients at 500 conns would
thrash. docker-compose carries laptop-scaled parity flags.

### Kafka — `deploy/helm-values/kafka.yaml`
KRaft mode (ZooKeeper quorum removed), 8 network / 16 I/O threads, 1 MiB
socket buffers, 1 GiB log segments, producer-side compression preserved.
Topic ACLs unchanged (tenant isolation intact).

### Fluvio — `deploy/helm-values/fluvio.yaml`
SPU replicas 2 → 4 (partition-leader parallelism for edge ingest).

### TigerBeetle — `deploy/kubernetes/tigerbeetle-statefulset.yaml`
3 → **6 replicas** (upstream production guidance; quorum survives two
failures). Ledger-per-tenant sharding already gives horizontal scale: total
ledger throughput = ~1M/s × number of clusters.

### Redis — `deploy/helm-values/redis.yaml`
Cache-only posture made explicit: **persistence off** (`save ""`, no AOF),
`io-threads 4` with threaded reads, lazy-free eviction/expire, 20k maxclients.
Nothing here is a source of truth — eviction is always safe.

### Temporal — `deploy/helm-values/temporal.yaml` + worker code
`numHistoryShards: 4096` — **immutable after bootstrap; must be set before
first install on a cluster**. Dynamic-config RPS guardrails. Python workers
now run `max_concurrent_activities=100` / `max_concurrent_workflow_tasks=200`
(activities are I/O-bound).

### Permify — `deploy/permify/schema.perm`
Schema unchanged (correctness layer). Perf guidance: run 3+ pods behind the
service, keep the Postgres backend on the same tuned cluster, enable
Permify's in-memory schema cache (default) — authorization checks are a
read-mostly lookup, not the bottleneck.

### APISIX — `deploy/helm-values/apisix.yaml`
`worker_processes: auto`, 65k connections per worker, HTTP/2, 75 s keepalive,
sized lua_shared_dicts for limit-count and prometheus. Autoscale 3→12 at 65%
CPU unchanged.

### OpenSearch — `deploy/helm-values/opensearch.yaml`
Heap pinned 8 GiB (compressed-oops boundary), write queue 1000, index buffer
20%, query cache 10%. ISM retention policy unchanged (6-year federal mandate).

### Lakehouse (Spark/Delta) — `deploy/helm-values/spark.yaml`
Workers 3 → 6 (4–8 vCPU, 8–16 GiB). Job conf (analytics-py submit):
`spark.sql.adaptive.enabled=true` (AQE fixes shuffle skew),
`spark.sql.shuffle.partitions=1024`, `spark.sql.files.maxPartitionBytes=256m`,
Delta `optimizeWrite` + `autoCompact`.

### Dapr — `deploy/dapr/`
`configuration.yaml` (new): 1% head-based trace sampling (full tracing taxes
every sidecar hop), low-cardinality metrics, mTLS kept on, deny-by-default
access control. `resiliency.yaml` (new): the timeouts/retries/circuit-breakers
the architecture doc promised — vault and case-api get circuit breakers so an
outage doesn't cascade; pub/sub publishes retry forever (outbox events must
not drop) while Redis cache calls get bounded retries (misses are recoverable).
Kafka pubsub component: `initialOffset=oldest`, version pinned to the cluster.
Sidecar resource annotations (cpu 100m→1, mem 128Mi→512Mi, 16M body,
32K read buffer) documented in `configuration.yaml` — default 250m/100Mi
throttles under pub/sub fan-out bursts.

### Autoscaling — KEDA (`deploy/kubernetes/keda-scaledobjects.yaml`)
Event-driven scaling on **work depth**, not just CPU: the outbox relay scales
1→8 replicas on unpublished outbox rows (PostgreSQL scaler, ~500 rows per
replica, scale-to-min when idle; `FOR UPDATE SKIP LOCKED` makes multi-replica
claiming safe). Kafka-lag scaler template included for future consumers.
KEDA itself ships via the `keda` helmfile release (idre-obs namespace).

### App manifests — `deploy/kubernetes/apps.yaml` (new)
Production Deployments for case-api / idre-workflows / outbox-relay with the
Dapr sidecar performance annotations from `configuration.yaml` (sized
sidecars, 16M body, streaming request bodies). The outbox relay runs **no
sidecar** — it speaks Kafka directly with the tuned producer (Tier-2 pattern
already applied where it matters most).

### Go (case-api)
Explicit pgxpool config: 50 max / 10 min conns (env-overridable via
`DB_POOL_MAX_CONNS`), 30 min conn lifetime, 30 s health checks. Combined with
the existing `ReadHeaderTimeout`, the server now fails fast instead of
queuing behind exhausted pools.

### Python (outbox relay)
Kafka producer: `linger.ms=5`, `batch.num.messages=10000`, `lz4`, 1 GiB
in-flight buffer, `max.in.flight=5` (idempotence ceiling). Claim batch 200 →
1000 with `FOR UPDATE SKIP LOCKED` (multi-replica safe).

### Rust (vault)
Explicit multi-thread Tokio runtime — the vault is CPU-bound on AEAD
seal/open; worker count must equal cores.

## Mojaloop and MySQL

**Does Mojaloop use MySQL?** Yes — its `central-ledger` service (the
transfer/position/fulfil core) is built on MySQL via the knex ORM. Several
peripheral services (account-lookup, quoting) also default to MySQL.

**Can it use Postgres instead?** Not realistically. knex abstracts the driver,
but central-ledger's migrations, stored procedures, and locking semantics are
written against MySQL/MariaDB; the upstream project does not publish or test a
Postgres profile. Running it on Postgres means forking and maintaining the
persistence layer yourself — not production-sane.

**Can MySQL be tuned?** Absolutely, and this is the standard path. If Mojaloop
is ever adopted as the settlement switch, front it with a tuned MySQL 8
(InnoDB cluster):

```ini
[mysqld]
innodb_buffer_pool_size        = 32G        # 60–70% of dedicated node RAM
innodb_log_file_size           = 4G         # fewer redo stalls on write bursts
innodb_flush_log_at_trx_commit = 1          # keep 1 — financial durability
innodb_flush_method            = O_DIRECT
innodb_io_capacity             = 4000       # NVMe
innodb_io_capacity_max         = 8000
innodb_flush_neighbors         = 0          # SSD
innodb_read_io_threads         = 8
innodb_write_io_threads        = 8
sync_binlog                    = 1
binlog_group_commit_sync_delay = 100        # µs — group-commit batching
transaction_isolation          = READ-COMMITTED  # Mojaloop's tested level
```

**For this platform today the question is moot:** Mojaloop is not deployed
(see `docs/PAYMENTS.md` — it is a reference architecture only). All durable
money movement is TigerBeetle (no MySQL anywhere in the stack). If real-money
rails are added later, the honest options are: (a) adopt Mojaloop with its
MySQL and tune it as above, or (b) keep TigerBeetle as the system of record
and integrate rails via a scheme adapter, avoiding MySQL entirely.

## Verification checklist

- [ ] PgBouncer sidecar deployed before `max_connections=500` goes live
- [ ] Temporal `numHistoryShards` set **at bootstrap** (cannot change later)
- [ ] TigerBeetle `format` re-run for the 6-replica cluster ID set
- [ ] Kafka KRaft migration is a fresh-cluster operation (or use Strimzi's
      ZK→KRaft migration path on a maintenance window)
- [ ] Load test: `k6` against APISIX, watch `pg_stat_statements` top-N and
      TigerBeetle `tigerbeetle_transfers_total`
