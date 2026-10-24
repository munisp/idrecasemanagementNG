# Author: Patrick Munis at newwave
"""PyFlink streaming: Kafka -> bronze Parquet (exactly-once) and a streaming
SLA early-warning job whose clocks come from each tenant's Program Manifest.

Bronze layout (contract with the batch tier — spark_gold reads this):
    s3a://lakehouse/bronze-raw/<domain>/tenant=<st>/date=YYYY-MM-DD/
One bulk-format Parquet sink per (domain, tenant) so tenant isolation is
physical, not just a column value, and Spark prunes by both tenant and date.
At 50 tenants x 6 domains that is 300 writers; each is idle until its
tenant produces, but if writer count becomes a scheduler problem the
escape hatch is a single sink per domain with the DateTimeBucketAssigner
alone (tenant stays a row column; silver MERGE already filters by it).

Domains: cases, offers, fees, rules, documents, AUDIT. Audit events go to
their own append-only bronze domain — the immutable 6-year archive the
architecture mandates; nothing in the batch tier ever updates it.

Alerts go to the shared ops topic ``idre.alerts`` (KafkaSink) — the
provisioner (scripts/provision-tenant.sh) grants the flink principal produce
rights on it; tenant isolation is preserved by the payload's tenant field.
"""

import json
import os
import time

try:  # the pure pieces (SLAWatch, to_row, DOMAIN_BY_PREFIX) must import
    # without a Flink runtime so CI can unit-test them; runtime paths that
    # need these names only execute under main()/operator functions.
    from pyflink.datastream import StreamExecutionEnvironment
    from pyflink.datastream.connectors.kafka import (
        KafkaSource, KafkaOffsetsInitializer, KafkaSink,
        KafkaRecordSerializationSchema,
    )
    from pyflink.datastream.connectors.file_system import (
        DateTimeBucketAssigner, FileSink, OutputFileConfig, RollingPolicy,
    )
    from pyflink.common.serialization import SimpleStringSchema
    from pyflink.common.watermark_strategy import WatermarkStrategy
    from pyflink.common import Types
except ImportError:  # pragma: no cover - CI without pyflink
    pass

BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092")
LAKEHOUSE = os.environ.get("LAKEHOUSE_BUCKET", "s3a://lakehouse")
DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@postgres:5432/idre")

# Warn this far ahead of a statutory deadline (processing-time timers make
# the warning fire even when no new event arrives for the case — the old
# version evaluated the deadline only on the initiation event, i.e. never).
WARN_AHEAD_DAYS = 2

# NSA defaults; manifest clocks override per tenant (see TenantClocks).
DEFAULT_CLOCKS = {
    "response_window": (3, "business"),
    "offer_window": (10, "business"),
    "selection_window": (10, "calendar"),
    "determination_window": (30, "business"),
    "payment_window": (30, "calendar"),
}

# Payload date fields the watcher tracks, in the event JSON.
DEADLINE_FIELDS = ("offer_window_ends_at", "open_negotiation_end")


class TenantClocks:
    """Fresh manifest clocks per tenant, refreshed from Postgres periodically
    (validate-on-write in case-api is the integrity gate; here we just read)."""

    REFRESH_SECONDS = 300

    def __init__(self) -> None:
        self._cache: dict[str, dict] = {}
        self._loaded_at = 0.0

    def _reload(self) -> None:
        import psycopg
        try:
            with psycopg.connect(DSN, autocommit=True) as c:
                rows = c.execute(
                    "SELECT tenant, config->'manifest'->'clocks'"
                    " FROM public.program_rules WHERE config ? 'manifest'"
                ).fetchall()
            cache: dict[str, dict] = {}
            for tenant, clocks in rows:
                resolved = dict(DEFAULT_CLOCKS)
                for item in clocks or []:
                    if (isinstance(item, dict)
                            and item.get("name") in resolved
                            and isinstance(item.get("days"), int) and item["days"] > 0
                            and item.get("day_type") in ("business", "calendar")):
                        resolved[item["name"]] = (item["days"], item["day_type"])
                cache[tenant] = resolved
            self._cache = cache
            self._loaded_at = time.monotonic()
        except Exception as e:  # fail-open: keep last good cache
            print(f"flink: manifest clock reload failed: {e}", flush=True)

    def for_tenant(self, tenant: str) -> dict:
        if time.monotonic() - self._loaded_at > self.REFRESH_SECONDS:
            self._reload()
        return self._cache.get(tenant, DEFAULT_CLOCKS)


CLOCKS = TenantClocks()


def bronze_sink(domain: str, tenant: str):
    """Bulk-format Parquet sink, checkpoint-committed (exactly-once),
    physically partitioned tenant=<st>/date=YYYY-MM-DD (hive-style dirs, so
    Spark partition-prunes both)."""
    from pyflink.datastream.formats.parquet import ParquetWriterFactory
    from pyflink.common import Types as T

    row_type = T.ROW_NAMED(["event_time", "tenant", "domain", "payload"],
                           [T.STRING(), T.STRING(), T.STRING(), T.STRING()])
    factory = ParquetWriterFactory(row_type)
    return (
        FileSink.for_bulk_format(f"{LAKEHOUSE}/bronze-raw/{domain}/tenant={tenant}", factory)
        .with_bucket_assigner(DateTimeBucketAssigner("'date='yyyy-MM-dd"))
        .with_output_file_config(OutputFileConfig.builder().with_part_prefix("events").build())
        .with_rolling_policy(RollingPolicy.default_rolling_policy())
        .build()
    )


# Event-type prefix -> bronze domain. Anything unmapped (e.g. sla.breach,
# ledger.balance_snapshot) lands in the "events" fallback domain so the
# batch tier can still route it to a silver table by exact type.
DOMAIN_BY_PREFIX = {
    "case": "cases",
    "offer": "offers",
    "fee": "fees",
    "rule": "rules",
    "doc": "documents",
    "audit": "audit",
}


def to_row(raw: str):
    """(event_time, tenant, domain, payload) — None on malformed JSON or a
    missing tenant (dropped by the downstream filter; bronze must never
    carry unparseable rows). Domain comes from the event-type prefix so the
    sinks below honor the bronze layout contract in the module docstring."""
    try:
        evt = json.loads(raw)
    except ValueError:
        return None
    tenant = evt.get("tenant", "")
    if not tenant:
        return None
    prefix = str(evt.get("type", "")).split(".", 1)[0]
    domain = DOMAIN_BY_PREFIX.get(prefix, "events")
    return (evt.get("at", ""), tenant, domain, raw)


class SLAWatch:
    """Pure deadline math, importable without a Flink runtime (unit-tested)."""

    WARN_MS = WARN_AHEAD_DAYS * 86_400_000

    @staticmethod
    def deadline_ms(iso_date: str) -> int | None:
        from datetime import date, datetime, timezone
        try:
            d = date.fromisoformat(str(iso_date)[:10])
        except (ValueError, TypeError):
            return None
        return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp() * 1000)

    @staticmethod
    def due(evt: dict, now_ms: int) -> list[tuple[str, int, str]]:
        """(clock_field, deadline_ms, iso) triples whose deadline is inside
        the warning window at now_ms."""
        out = []
        for field in DEADLINE_FIELDS:
            ms = SLAWatch.deadline_ms(evt.get(field) or "")
            if ms is not None and ms - now_ms <= SLAWatch.WARN_MS:
                out.append((field, ms, str(evt[field])[:10]))
        return out

    @staticmethod
    def next_timer_ms(evt: dict, now_ms: int) -> int | None:
        """Earliest warn-at instant still in the future, so a case that goes
        quiet still warns when its deadline enters the window."""
        soonest = None
        for field in DEADLINE_FIELDS:
            ms = SLAWatch.deadline_ms(evt.get(field) or "")
            if ms is not None and ms - now_ms > SLAWatch.WARN_MS:
                warn_at = ms - SLAWatch.WARN_MS
                soonest = warn_at if soonest is None else min(soonest, warn_at)
        return soonest

    @staticmethod
    def alert_payload(evt: dict, field: str, iso: str, case_id: str = "") -> str:
        tenant = evt.get("tenant", "")
        clock_name = "offer_window" if field == "offer_window_ends_at" else "response_window"
        days, day_type = CLOCKS.for_tenant(tenant).get(clock_name, (10, "business"))
        return json.dumps({
            "type": "sla.early_warning", "tenant": tenant,
            "case_id": evt.get("case_id") or case_id,
            "clock": f"{field.upper()}_{days}{'BD' if day_type == 'business' else 'CD'}",
            "deadline": iso,
            "emitted_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        })


def sla_watch_process():
    """KeyedProcessFunction (key = case_id). State per case: a JSON map with
    each tracked clock's deadline ISO plus the clocks already warned, so a
    (case, clock) pair warns exactly once across restarts (state is
    checkpointed) and a timer fires the warning even when no new event
    arrives for the case. Terminal events clear the state."""
    from pyflink.datastream.functions import KeyedProcessFunction
    from pyflink.datastream.state import ValueStateDescriptor

    class _Watch(KeyedProcessFunction):
        def open(self, runtime_context):
            self.state = runtime_context.get_state(
                ValueStateDescriptor("sla_watch", Types.STRING()))

        def process_element(self, value, ctx, out):
            try:
                evt = json.loads(value)
            except ValueError:
                return
            if not str(evt.get("type", "")).startswith("case."):
                return
            snap = json.loads(self.state.value() or "{}")
            if str(evt.get("status", "")).startswith("CLOSED"):
                self.state.clear()
                return
            now_ms = int(time.time() * 1000)
            # Persist what we know: tenant + every tracked deadline, so a
            # later timer has the full picture even if this event lacks one.
            snap["tenant"] = evt.get("tenant", "")
            deadlines = snap.setdefault("deadlines", {})
            for field in DEADLINE_FIELDS:
                iso = str(evt.get(field) or "")[:10]
                if iso:
                    deadlines[field] = iso
            warned = snap.setdefault("warned", {})
            # Warn immediately for anything already inside the window.
            for field, _ms, iso in SLAWatch.due(evt, now_ms):
                if warned.get(field) == iso:
                    continue
                warned[field] = iso
                out.append(SLAWatch.alert_payload(evt, field, iso))
            # Arm a timer for the first deadline still outside the window.
            soonest = SLAWatch.next_timer_ms(evt, now_ms)
            if soonest is not None:
                ctx.timer_service().register_processing_time_timer(soonest)
            self.state.update(json.dumps(snap))

        def on_timer(self, timestamp, ctx, out):
            snap = json.loads(self.state.value() or "{}")
            deadlines = snap.get("deadlines", {})
            warned = snap.setdefault("warned", {})
            now_ms = int(time.time() * 1000)
            case_id = str(ctx.get_current_key())
            evt = {"tenant": snap.get("tenant", ""), "case_id": case_id}
            for field, iso in deadlines.items():
                ms = SLAWatch.deadline_ms(iso)
                if ms is None or ms - now_ms > SLAWatch.WARN_MS:
                    continue
                if warned.get(field) == iso:
                    continue
                warned[field] = iso
                out.append(SLAWatch.alert_payload(evt, field, iso, case_id))
            self.state.update(json.dumps(snap))

    return _Watch()


def main() -> None:
    env = StreamExecutionEnvironment.get_execution_environment()
    env.enable_checkpointing(30_000)  # exactly-once baseline

    domains = ["cases", "offers", "fees", "rules", "documents", "audit"]
    states = os.environ.get("TENANT_STATES", "tx").split()
    topics = [f"idre.{st}.{d}" for st in states for d in domains]

    source = (
        KafkaSource.builder()
        .set_bootstrap_servers(BROKERS)
        .set_topics(*topics)
        .set_group_id("flink-bronze")
        .set_starting_offsets(KafkaOffsetsInitializer.earliest())
        .set_value_only_deserializer(SimpleStringSchema())
        .build()
    )
    stream = env.from_source(source, WatermarkStrategy.no_watermarks(), "kafka-bronze")

    rows = (
        stream.map(to_row, output_type=Types.TUPLE(
            [Types.STRING(), Types.STRING(), Types.STRING(), Types.STRING()]))
        .filter(lambda r: r is not None)
    )
    # One sink per (tenant, domain): physical tenant partition per the
    # bronze contract. Rows carry the tenant and domain columns too, so a
    # misrouted event is detectable at the batch tier rather than silently
    # misplaced.
    sink_domains = domains + ["events"]  # "events" = fallback for sla./ledger. types
    for st in states:
        for d in sink_domains:
            rows.filter(lambda r, st=st, d=d: r[1] == st and r[2] == d).sink_to(
                bronze_sink(d, st))

    # Early-warning stream -> shared ops topic, keyed by case so each
    # (case, clock) warns exactly once even across restarts (state is
    # checkpointed with everything else).
    case_events = stream.filter(lambda r: '"case_id"' in r)
    keyed = case_events.key_by(lambda r: json.loads(r).get("case_id", "") or "")
    warned = keyed.process(sla_watch_process(), output_type=Types.STRING())
    warned.sink_to(
        KafkaSink.builder()
        .set_bootstrap_servers(BROKERS)
        .set_record_serializer(
            KafkaRecordSerializationSchema.builder()
            .set_topic("idre.alerts")
            .set_value_serialization_schema(SimpleStringSchema())
            .build()
        )
        .build()
    )

    env.execute("idre-bronze+sla-early-warning")


if __name__ == "__main__":
    main()
