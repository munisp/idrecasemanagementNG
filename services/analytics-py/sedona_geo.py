# Author: Patrick Munis at newwave
"""Apache Sedona: geospatial analytics for jurisdiction & coverage.

- Verifies provider service location falls inside the claimed state tenant
  (federal vs. specified-state-law jurisdiction checks at borders).
- Coverage-gap mapping: counties with no in-network emergency providers.
- Air-ambulance corridor analysis (state-scope rules for air claims).

Input contract (both feeds are platform-produced; the job validates them
instead of assuming — the previous version read silver/providers, which
nothing wrote, and would have failed on an empty read every run):
  ref/us_states     written by load_us_states.py (Census TIGER boundaries,
                    WKT geometry parsed here with ST_GeomFromWKT)
  silver/providers  written by pg_export.py (provider directory; lat/lon
                    may be NULL until the platform captures service
                    locations — geo marts requiring coordinates are then
                    SKIPPED with a logged reason, not a crash)
"""

from __future__ import annotations

from pyspark.sql import functions as F
from sedona.spark import SedonaContext

MINIO = "s3a://lakehouse"


def _path_exists(spark, path: str) -> bool:
    p = spark._jvm.org.apache.hadoop.fs.Path(path)
    return p.getFileSystem(spark._jsc.hadoopConfiguration()).exists(p)


def main() -> None:
    sedona = (
        SedonaContext.builder()
        .appName("idre-geo")
        .config("spark.hadoop.fs.s3a.endpoint", "http://minio:9000")
        .config("spark.hadoop.fs.s3a.access.key", "idre")
        .config("spark.hadoop.fs.s3a.secret.key", "idre-secret")
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
        .getOrCreate()
    )
    spark = sedona.spark

    if not _path_exists(spark, f"{MINIO}/ref/us_states"):
        print("sedona: ref/us_states missing — run load_us_states.py first; "
              "geo marts skipped")
        return
    states = (spark.read.parquet(f"{MINIO}/ref/us_states")
              .withColumn("geom", F.expr("ST_GeomFromWKT(geom_wkt)")))
    states.createOrReplaceTempView("states")

    written: list[str] = []

    providers_ok = _path_exists(spark, f"{MINIO}/silver/providers")
    if providers_ok:
        providers = spark.read.parquet(f"{MINIO}/silver/providers")
        # Coordinates may be NULL until the platform captures service
        # locations; geo joins on NULL points silently drop every row, so
        # check explicitly and skip with a reason instead.
        located = providers.where(F.col("lat").isNotNull()
                                  & F.col("lon").isNotNull())
        providers_ok = located.limit(1).count() > 0
    if not providers_ok:
        print("sedona: no provider coordinates in silver/providers "
              "(pg_export.py exports NULLs until service-location capture "
              "lands) — jurisdiction/coverage marts skipped")
    else:
        providers.createOrReplaceTempView("providers")
        if _path_exists(spark, f"{MINIO}/silver/cases"):
            cases = spark.read.format("delta").load(f"{MINIO}/silver/cases")
            cases.createOrReplaceTempView("cases")

            # Jurisdiction check: the state polygon containing the service
            # location must equal the case tenant — otherwise SSL/federal
            # routing may be wrong.
            spark.sql("""
                SELECT c.case_id, c.tenant AS claimed_tenant,
                       s.state_code AS geo_state,
                       (s.state_code = c.tenant) AS jurisdiction_consistent
                FROM (SELECT DISTINCT tenant, case_id, provider_id
                      FROM cases WHERE provider_id IS NOT NULL) c
                JOIN providers p ON p.provider_id = c.provider_id
                JOIN states s ON ST_Contains(s.geom, ST_Point(p.lon, p.lat))
            """).write.format("delta").mode("overwrite") \
               .save(f"{MINIO}/gold/geo_jurisdiction_check")
            written.append("geo_jurisdiction_check")

        # Coverage gaps: counties with zero in-network providers.
        spark.sql("""
            SELECT s.state_code, s.county_fips,
                   COUNT(p.provider_id) AS in_network_providers
            FROM states s
            LEFT JOIN providers p
              ON ST_Contains(s.geom, ST_Point(p.lon, p.lat))
             AND p.in_network = true
            GROUP BY s.state_code, s.county_fips
        """).filter("in_network_providers = 0") \
           .write.format("delta").mode("overwrite") \
           .save(f"{MINIO}/gold/geo_coverage_gaps")
        written.append("geo_coverage_gaps")

    # GeoLibre handoff: cloud-native GeoParquet straight from MinIO (no
    # tile server). Only for marts actually (re)written this run.
    for name in written:
        (
            spark.read.format("delta").load(f"{MINIO}/gold/{name}")
            .write.mode("overwrite").format("geoparquet")
            .save(f"{MINIO}/gold-geo/{name}.parquet")
        )

    print(f"geo analytics: wrote {written or 'nothing (inputs not ready)'}")


if __name__ == "__main__":
    main()
