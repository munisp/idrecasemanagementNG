"""PyFlink streaming: Kafka -> bronze Parquet (exactly-once) and a streaming
SLA early-warning job whose clocks come from each tenant's Program Manifest.

Contract with the batch tier: bronze files are Parquet under
s3a://lakehouse/bronze-raw/<domain>/date=YYYY-MM-DD/ (bulk format sink);
spark_gold.bronze_to_silver reads Parquet and MERGEs into Delta silver.

Alerts go to the shared ops topic ``idre.alerts`` (KafkaSink) — the
provisioner (scripts/provision-tenant.sh) grants the flink principal produce
rights on it; tenant isolation is preserved by the payload's tenant field.
"""

import json
import os
import time

from pyflink.datastream import StreamExecutionEnvironment
from pyflink.datastream.connectors.kafka import (
    KafkaSource, KafkaOffsetsInitializer, KafkaSink,
    KafkaRecordSerializationSchema,
)
from pyflink.datastream.connectors.file_system import (
    FileSink, OutputFileConfig, RollingPolicy,
)
from pyflink.common.serialization import SimpleStringSchema
from pyflink.common.watermark_strategy import WatermarkStrategy
from pyflink.common import Types

BROKERS = os.environ.get("KAFKA_BROKERS", "kafka:9092")
LAKEHOUSE = os.environ.get("LAKEHOUSE_BUCKET", "s3a://lakehouse")
DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@postgres:5432/idre")

# NSA defaults; manifest clocks override per tenant (see manifestClocks).
DEFAULT_CLOCKS = {
    "response_window": (3, "business"),
    "offer_window": (10, "business"),
    "selection_window": (10, "calendar"),
    "determination_window": (30, "business"),
    "payment_window": (30, "calendar"),
}


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


def bronze_sink(domain: str) -> FileSink:
    """Bulk-format Parquet sink, checkpoint-committed (exactly-once)."""
    from pyflink.datastream.formats.parquet import ParquetWriterFactory
    from pyflink.common import Types as T

    row_type = T.ROW_NAMED(["event_time", "tenant", "payload"],
                           [T.STRING(), T.STRING(), T.STRING()])
    factory = ParquetWriterFactory(row_type)
    return (
        FileSink.for_bulk_format(f"{LAKEHOUSE}/bronze-raw/{domain}", factory)
        .with_output_file_config(OutputFileConfig.builder().with_part_prefix("events").build())
        .with_rolling_policy(RollingPolicy.default_rolling_policy())
        .build()
    )


def to_row(domain: str):
    def _map(raw: str):
        try:
            evt = json.loads(raw)
        except ValueError:
            return None
        return (evt.get("at", ""), evt.get("tenant", ""), raw)
    return _map


def breach_candidates(raw: str):
    """Emit an early warning when a case's offer window is < 2 days away.
    Window length comes from the tenant manifest (offer_window clock)."""
    from datetime import date, datetime, timedelta

    try:
        evt = json.loads(raw)
    except ValueError:
        return []
    if evt.get("type") != "case.initiated" or not evt.get("offer_window_ends_at"):
        return []
    tenant = evt.get("tenant", "")
    days, _day_type = CLOCKS.for_tenant(tenant).get("offer_window", (10, "business"))
    try:
        deadline = date.fromisoformat(str(evt["offer_window_ends_at"])[:10])
    except ValueError:
        return []
    if (deadline - date.today()).days <= 2:
        yield json.dumps({
            "type": "sla.early_warning", "tenant": tenant,
            "case_id": evt.get("case_id"),
            "clock": f"OFFER_WINDOW_{days}{'BD' if _day_type == 'business' else 'CD'}",
            "deadline": deadline.isoformat(),
            "emitted_at": datetime.utcnow().isoformat() + "Z",
        })


def main() -> None:
    env = StreamExecutionEnvironment.get_execution_environment()
    env.enable_checkpointing(30_000)  # exactly-once baseline

    domains = ["cases", "offers", "fees", "rules", "documents"]
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
        stream.map(lambda r: to_row("")(r), output_type=Types.TUPLE(
            [Types.STRING(), Types.STRING(), Types.STRING()]))
        .filter(lambda r: r is not None)
    )
    rows.sink_to(bronze_sink("events"))

    # Early-warning stream -> shared ops topic (real sink, not stdout).
    alerts = stream.flat_map(
        lambda r: list(breach_candidates(r)), output_type=Types.STRING()
    )
    alerts.sink_to(
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
