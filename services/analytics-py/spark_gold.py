"""Spark batch: bronze -> silver -> gold, Delta Lake on MinIO/S3.

Incremental and idempotent by construction:
  - Every bronze row is keyed by a content fingerprint — sha256 of
    tenant|event_time|payload, exactly lakehouse_common.event_fingerprint —
    and silver MERGEs on it, so outbox replays, Kafka redeliveries, and
    overlapping windows are harmless (the old full-scan overwrite dropped
    anything not in the current bronze read).
  - silver/_control records the high-water event_time per tenant. Each run
    reads only bronze rows newer than the watermark and advances it AFTER
    every silver merge succeeds; a mid-run failure leaves the watermark
    behind, so the next run re-reads and the fingerprint MERGE dedupes.
  - bronze-raw/audit is NEVER touched here: it is the immutable 6-year
    archive; nothing in the batch tier updates it.
  - The CMS monthly report scopes EVERY metric by event month via
    lakehouse_common.month_bounds (closures count in the month they closed,
    not the month the case happened to open — the old report mis-scoped
    closures, breaches, and fees to the opening month's cohort).
  - No VACUUM on silver: retention is a 6-year legal mandate, so the job
    OPTIMIZEs but never deletes record files. Gold marts are regenerable
    and may be vacuumed.

Run: python spark_gold.py <tenant> [YYYY-MM] [--full]
--full replays all bronze history for the tenant (fingerprint MERGE keeps
it idempotent); without it, only rows newer than the watermark are read.
"""

from __future__ import annotations

import sys
from datetime import date

from delta import configure_spark_with_delta_pip
from delta.tables import DeltaTable
from pyspark.sql import SparkSession, DataFrame, functions as F

from lakehouse_common import month_bounds

MINIO = "s3a://lakehouse"

# bronze domain -> silver table. "events" is the fallback domain for event
# types without their own domain (sla.breach, ledger.balance_snapshot);
# those split into typed silver tables by exact etype below.
DOMAIN_TABLES = {
    "cases": "cases",
    "offers": "offers",
    "fees": "fees",
    "rules": "rule_events",
    "documents": "document_events",
}
EVENTS_FALLBACK_TABLES = {  # exact etype -> silver table, from bronze-raw/events
    "sla.breach": "sla_breaches",
    "ledger.balance_snapshot": "ledger_balances",
}

# Per-cell sample floor for gold benchmarks — same discipline as the
# case-api calibrateMinShown gate: a cell below the floor is not published.
QPA_MIN_CELL = 30


def spark() -> SparkSession:
    builder = (
        SparkSession.builder.appName("idre-gold")
        .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
        .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
        # LEGACY parser returns NULL on a failed timestamp parse, so the
        # COALESCE chain in _iso_ts can fall through candidate ISO formats
        # instead of aborting the whole batch on one odd row.
        .config("spark.sql.legacy.timeParserPolicy", "LEGACY")
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000")
        .config("spark.hadoop.fs.s3a.access.key", "idre")
        .config("spark.hadoop.fs.s3a.secret.key", "idre-secret")
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
    )
    return configure_spark_with_delta_pip(builder).getOrCreate()


def _path_exists(s: SparkSession, path: str) -> bool:
    p = s._jvm.org.apache.hadoop.fs.Path(path)
    return p.getFileSystem(s._jsc.hadoopConfiguration()).exists(p)


# ---------------------------------------------------------------------------
# Watermark control
# ---------------------------------------------------------------------------

CONTROL = f"{MINIO}/silver/_control"


def _watermark(s: SparkSession, tenant: str) -> str:
    """High-water event_time already merged for this tenant ("" = none)."""
    if not _path_exists(s, CONTROL):
        return ""
    rows = (s.read.format("delta").load(CONTROL)
            .where(F.col("tenant") == tenant)
            .select("watermark").collect())
    return rows[0]["watermark"] if rows else ""


def _advance_watermark(s: SparkSession, tenant: str, watermark: str) -> None:
    if not _path_exists(s, CONTROL):
        (s.createDataFrame([(tenant, watermark)], "tenant string, watermark string")
           .write.format("delta").save(CONTROL))
        return
    src = s.createDataFrame([(tenant, watermark)], "tenant string, watermark string")
    (DeltaTable.forPath(s, CONTROL).alias("t")
       .merge(src.alias("s"), "t.tenant = s.tenant")
       .whenMatchedUpdateAll()
       .whenNotMatchedInsertAll()
       .execute())


# ---------------------------------------------------------------------------
# Bronze -> silver
# ---------------------------------------------------------------------------


def _iso_ts(col: str):
    """Parse ISO-8601 event_time ('Z' or numeric offset, optional millis)."""
    return F.coalesce(
        F.to_timestamp(F.col(col), "yyyy-MM-dd'T'HH:mm:ss.SSS'Z'"),
        F.to_timestamp(F.col(col), "yyyy-MM-dd'T'HH:mm:ss'Z'"),
        F.to_timestamp(F.col(col), "yyyy-MM-dd'T'HH:mm:ss.SSSXXX"),
        F.to_timestamp(F.col(col), "yyyy-MM-dd'T'HH:mm:ssXXX"),
    )


def _common(df: DataFrame) -> DataFrame:
    """Typed columns parsed once from the raw JSON payload, plus the content
    fingerprint. F.sha2(concat(tenant,'|',event_time,'|',payload),256) is
    byte-identical to lakehouse_common.event_fingerprint — parity is pinned
    by unit test so bronze identity survives engine rewrites."""
    j = lambda field: F.get_json_object("payload", f"$.{field}")
    return (
        df.withColumn(
            "fingerprint",
            F.sha2(F.concat("tenant", F.lit("|"), "event_time", F.lit("|"), "payload"), 256))
        .withColumn("etype", j("type"))
        .withColumn("case_id", j("case_id"))
        .withColumn("provider_id", j("provider_id"))
        .withColumn("payer_id", j("payer_id"))
        .withColumn("status", j("status"))
        .withColumn("service_line", j("service_line"))
        .withColumn("event_at", _iso_ts("event_time"))
    )


def _merge(s: SparkSession, df: DataFrame, table: str) -> None:
    """Insert-only MERGE keyed on fingerprint: replays are no-ops."""
    path = f"{MINIO}/silver/{table}"
    if not _path_exists(s, path):
        df.write.format("delta").partitionBy("tenant").save(path)
        return
    (DeltaTable.forPath(s, path).alias("t")
       .merge(df.alias("n"), "t.fingerprint = n.fingerprint")
       .whenNotMatchedInsertAll()
       .execute())


def _bronze_domain(s: SparkSession, domain: str, tenant: str, watermark: str) -> DataFrame | None:
    """Read one bronze domain for one tenant, partition-pruned by reading the
    tenant path directly (tenant is both a row column and a hive partition
    dir; reading the domain root would collide the two)."""
    base = f"{MINIO}/bronze-raw/{domain}/tenant={tenant}"
    if not _path_exists(s, base):
        return None
    df = s.read.option("basePath", base).parquet(base)
    df = df.where(F.col("event_time") > F.lit(watermark))
    return df


def bronze_to_silver(s: SparkSession, tenant: str, full: bool = False) -> None:
    watermark = "" if full else _watermark(s, tenant)
    max_seen = watermark

    domains = dict(DOMAIN_TABLES)
    for domain, table in domains.items():
        raw = _bronze_domain(s, domain, tenant, watermark)
        if raw is None:
            continue
        df = _common(raw).where(F.col("event_time") != "").cache()
        if df.limit(1).count():
            _merge(s, df, table)
            m = df.agg(F.max("event_time")).first()[0]
            if m and m > max_seen:
                max_seen = m
        df.unpersist()

    # Fallback domain: route by exact etype into typed silver tables.
    raw = _bronze_domain(s, "events", tenant, watermark)
    if raw is not None:
        ev = _common(raw).where(F.col("event_time") != "").cache()
        if ev.limit(1).count():
            for etype, table in EVENTS_FALLBACK_TABLES.items():
                part = ev.where(F.col("etype") == etype)
                if part.limit(1).count():
                    _merge(s, part, table)
            m = ev.agg(F.max("event_time")).first()[0]
            if m and m > max_seen:
                max_seen = m
        ev.unpersist()

    # Advance last: a failure above leaves the watermark behind, so the
    # next run re-reads and the fingerprint MERGE absorbs the duplicate.
    if max_seen != watermark:
        _advance_watermark(s, tenant, max_seen)
        print(f"silver watermark {tenant}: {watermark or '-'} -> {max_seen}")


def _silver(s: SparkSession, table: str, tenant: str) -> DataFrame:
    return (s.read.format("delta").load(f"{MINIO}/silver/{table}")
            .where(F.col("tenant") == tenant))


# ---------------------------------------------------------------------------
# Gold marts
# ---------------------------------------------------------------------------


def _write_gold(s: SparkSession, df: DataFrame, mart: str, where: str,
                partition_by: str = "tenant") -> str:
    path = f"{MINIO}/gold/{mart}"
    if not _path_exists(s, path):
        df.write.format("delta").partitionBy(partition_by).save(path)
    else:
        (df.write.format("delta").mode("overwrite")
           .option("replaceWhere", where)
           .save(path))
    return path


def cms_monthly_report(s: SparkSession, tenant: str, month: str) -> str:
    """45 CFR 149 monthly report. Every metric is scoped by EVENT month:
    disputes opened in the month, closures that happened in the month,
    breaches raised in the month, fee volume dated in the month."""
    first, nxt = month_bounds(month)
    lo, hi = F.lit(first.isoformat()), F.lit(nxt.isoformat())
    in_month = (F.col("event_at") >= lo.cast("timestamp")) & \
               (F.col("event_at") < hi.cast("timestamp"))

    cases = _silver(s, "cases", tenant)
    opened = cases.where((F.col("etype") == "case.initiated") & in_month)
    qpa_usd = F.get_json_object("payload", "$.qpa_cents").cast("double") / 100
    opened_agg = opened.agg(
        F.countDistinct("case_id").alias("disputes_initiated"),
        F.avg(qpa_usd).alias("avg_qpa_usd"),
    )

    closed = cases.where(
        in_month & (F.col("status").startswith("CLOSED")
                    | (F.col("status") == "SETTLED_IN_NEGOTIATION")))
    closed_agg = closed.agg(
        F.countDistinct("case_id").alias("closed_in_month"),
        F.countDistinct(F.when(F.col("status") == "CLOSED_PAID", F.col("case_id")))
         .alias("closed_paid_in_month"),
    )

    fees = _silver(s, "fees", tenant).where(in_month).agg(
        F.sum(F.get_json_object("payload", "$.amount_cents").cast("long"))
         .alias("fee_volume_cents"))

    breach_n = 0
    if _path_exists(s, f"{MINIO}/silver/sla_breaches"):
        breach_n = (_silver(s, "sla_breaches", tenant).where(in_month)
                    .agg(F.count("*").alias("n")).first()["n"])

    row = (opened_agg.crossJoin(closed_agg).crossJoin(fees)
           .withColumn("sla_breaches", F.lit(breach_n))
           .withColumn("tenant", F.lit(tenant))
           .withColumn("month", F.lit(month)))
    return _write_gold(s, row, "cms_monthly_report",
                       f"tenant = '{tenant}' AND month = '{month}'",
                       partition_by="tenant")


def gold_marts(s: SparkSession, tenant: str) -> None:
    """Derived gold marts, all tenant-scoped, all sample-gated where a cell
    could quote a number it hasn't earned."""
    # SLA breach profile: per clock, per month.
    if _path_exists(s, f"{MINIO}/silver/sla_breaches"):
        br = (_silver(s, "sla_breaches", tenant)
              .withColumn("clock", F.get_json_object("payload", "$.clock"))
              .withColumn("month", F.date_format("event_at", "yyyy-MM"))
              .groupBy("tenant", "clock", "month")
              .agg(F.count("*").alias("breaches"),
                   F.avg(F.get_json_object("payload", "$.hours_overdue")
                         .cast("double")).alias("avg_hours_overdue")))
        _write_gold(s, br, "sla_breach_summary", f"tenant = '{tenant}'")

    # QPA benchmarks per service line — published only where n >= floor.
    qpa_usd = F.get_json_object("payload", "$.qpa_cents").cast("double") / 100
    bench = (
        _silver(s, "cases", tenant)
        .where((F.col("etype") == "case.initiated") & qpa_usd.isNotNull()
               & (F.col("service_line") != ""))
        .groupBy("tenant", "service_line")
        .agg(F.count("*").alias("n"),
             F.expr("percentile_approx(CAST(get_json_object(payload, '$.qpa_cents') AS DOUBLE) / 100, "
                    "array(0.25, 0.5, 0.75))").alias("qpa_p25_p50_p75_usd"),
             F.avg(qpa_usd).alias("avg_qpa_usd"))
        .where(F.col("n") >= QPA_MIN_CELL))
    _write_gold(s, bench, "qpa_benchmarks", f"tenant = '{tenant}'")

    # Settlement positions: latest ledger balance snapshot per case.
    if _path_exists(s, f"{MINIO}/silver/ledger_balances"):
        from pyspark.sql.window import Window
        w = (Window.partitionBy("tenant", "case_id")
             .orderBy(F.col("event_at").desc_nulls_last(), F.col("event_time").desc()))
        latest = (
            _silver(s, "ledger_balances", tenant)
            .withColumn("rn", F.row_number().over(w))
            .where(F.col("rn") == 1).drop("rn")
            .withColumn("balance_cents",
                        F.get_json_object("payload", "$.balance_cents").cast("long"))
            .withColumn("currency", F.get_json_object("payload", "$.currency")))
        _write_gold(s, latest, "settlement_positions", f"tenant = '{tenant}'")


def optimize_and_vacuum(s: SparkSession) -> None:
    """OPTIMIZE every silver table that exists. NO VACUUM on silver: the
    6-year retention mandate means record files are never deleted by a
    scheduled job (a previous version vacuumed cases at 720h, which would
    have destroyed records well inside the mandate). Gold is regenerable,
    so a default-retention vacuum there is safe."""
    tables = list(DOMAIN_TABLES.values()) + list(EVENTS_FALLBACK_TABLES.values())
    for table in tables:
        path = f"{MINIO}/silver/{table}"
        if _path_exists(s, path):
            s.sql(f"OPTIMIZE delta.`{path}` ZORDER BY (tenant, case_id)")
    for mart in ("cms_monthly_report", "sla_breach_summary",
                 "qpa_benchmarks", "settlement_positions"):
        path = f"{MINIO}/gold/{mart}"
        if _path_exists(s, path):
            s.sql(f"VACUUM delta.`{path}`")  # default 168h; gold is regenerable


if __name__ == "__main__":
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    tenant = args[0]
    month = args[1] if len(args) > 1 else date.today().strftime("%Y-%m")
    sess = spark()
    bronze_to_silver(sess, tenant, full="--full" in sys.argv)
    print(cms_monthly_report(sess, tenant, month))
    gold_marts(sess, tenant)
    optimize_and_vacuum(sess)
