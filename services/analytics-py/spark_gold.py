"""Spark batch: bronze -> silver -> gold, Delta Lake on MinIO/S3.
Generates the CMS monthly report per tenant (45 CFR 149 reporting mandate)."""

import sys
from datetime import date

from delta import configure_spark_with_delta_pip
from pyspark.sql import SparkSession, functions as F

MINIO = "s3a://lakehouse"


def spark() -> SparkSession:
    builder = (
        SparkSession.builder.appName("idre-gold")
        .config("spark.sql.extensions", "io.delta.sql.DeltaSparkSessionExtension")
        .config("spark.sql.catalog.spark_catalog", "org.apache.spark.sql.delta.catalog.DeltaCatalog")
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000")
        .config("spark.hadoop.fs.s3a.access.key", "idre")
        .config("spark.hadoop.fs.s3a.secret.key", "idre-secret")
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
    )
    return configure_spark_with_delta_pip(builder).getOrCreate()


def bronze_events(s: SparkSession, tenant: str):
    """Bronze = Parquet event archive from Flink (bronze-raw/events/date=...).
    Silver tables carry typed common columns + the raw JSON payload: robust to
    schema drift across domains; gold marts parse what they need."""
    return (s.read.parquet(f"{MINIO}/bronze-raw/events")
            .where(F.col("tenant") == tenant))


def _silver(df, table: str, tenant: str) -> None:
    (df.write.format("delta").mode("overwrite")
       .option("replaceWhere", f"tenant = '{tenant}'")
       .save(f"{MINIO}/silver/{table}"))


def _common(df):
    return (df.withColumn("etype", F.get_json_object("payload", "$.type"))
              .withColumn("case_id", F.get_json_object("payload", "$.case_id")))


def bronze_to_silver(s: SparkSession, tenant: str) -> None:
    ev = _common(bronze_events(s, tenant)).cache()
    _silver(ev.where(F.col("etype").startswith("case.")), "cases", tenant)
    _silver(ev.where(F.col("etype").startswith("offer.")), "offers", tenant)
    _silver(ev.where(F.col("etype").startswith("fee.")), "fees", tenant)
    _silver(ev.where(F.col("etype").startswith("rule.")), "rule_events", tenant)
    _silver(ev.where(F.col("etype").startswith("doc.")), "document_events", tenant)
    # SLA breaches arrive via the tenant outbox (flag_cms_breach emits an
    # outbox event per breach); ledger snapshots come from the TB exporter.
    _silver(ev.where(F.col("etype") == "sla.breach"), "sla_breaches", tenant)
    _silver(ev.where(F.col("etype") == "ledger.balance_snapshot"), "ledger_balances", tenant)


def cms_monthly_report(s: SparkSession, tenant: str, month: str) -> str:
    cases = s.read.format("delta").load(f"{MINIO}/silver/cases").where(F.col("tenant") == tenant)
    fees = s.read.format("delta").load(f"{MINIO}/silver/fees").where(F.col("tenant") == tenant)
    breaches = s.read.format("delta").load(f"{MINIO}/silver/sla_breaches").where(F.col("tenant") == tenant)

    opened = F.to_date(F.get_json_object("payload", "$.opened_at"))
    status = F.get_json_object("payload", "$.status")
    qpa_usd = F.get_json_object("payload", "$.qpa_cents").cast("double") / 100
    report = (
        cases.where(F.date_trunc("month", opened) == F.lit(f"{month}-01").cast("date"))
        .groupBy("tenant").agg(
            F.count("*").alias("disputes_initiated"),
            F.sum(F.when(status == "CLOSED_PAID", 1).otherwise(0)).alias("closed_paid"),
            F.avg(qpa_usd).alias("avg_qpa_usd"),
        )
        .join(fees.groupBy("tenant").agg(
            F.sum(F.get_json_object("payload", "$.amount_cents").cast("long"))
             .alias("fee_volume_cents")), "tenant", "left")
        .join(breaches.groupBy("tenant").count().withColumnRenamed("count", "sla_breaches"),
              "tenant", "left")
    )
    key = f"{MINIO}/gold/cms_monthly_report/tenant={tenant}/month={month}"
    report.write.format("delta").mode("overwrite").save(key)
    return key


def optimize_and_vacuum(s: SparkSession) -> None:
    for table in ("cases", "offers", "fees"):
        s.sql(f"OPTIMIZE delta.`{MINIO}/silver/{table}` ZORDER BY (tenant, case_id)")
    # 6-year retention mandate: never vacuum beyond legal hold window
    s.sql(f"VACUUM delta.`{MINIO}/silver/cases` RETAIN 720 HOURS")  # metadata only; rows kept 6y


if __name__ == "__main__":
    tenant, month = sys.argv[1], sys.argv[2] if len(sys.argv) > 2 else date.today().strftime("%Y-%m")
    sess = spark()
    bronze_to_silver(sess, tenant)
    print(cms_monthly_report(sess, tenant, month))
    optimize_and_vacuum(sess)
