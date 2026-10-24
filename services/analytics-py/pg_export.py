# Author: Patrick Munis at newwave
"""Postgres -> lakehouse reference feeds (run before sedona_geo.py).

Exports the platform's provider/facility directory (public.entities,
entity_type PROVIDER|FACILITY) to silver/providers so the geo jobs have a
real input — the previous Sedona job read silver/providers and NOTHING ever
wrote it, so the geo marts could never materialize.

Coordinate columns (lat/lon, in_network) are exported as NULLs until the
platform captures service locations; the schema is fixed NOW so downstream
jobs and the Sedona defensive checks have a stable contract. Rows are
partitioned by tenant; the export is a full snapshot per tenant (the
directory is small and append-only in practice).

Run: python pg_export.py <tenant>
Env: DATABASE_URL, S3_* (same conventions as the other analytics jobs).
"""

from __future__ import annotations

import os
import sys

import pyarrow as pa
import pyarrow.fs as pafs
import pyarrow.parquet as pq

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@postgres:5432/idre")
S3_BUCKET = os.environ.get("LAKEHOUSE_BUCKET", "lakehouse")
S3_ENDPOINT = os.environ.get("S3_ENDPOINT", "minio:9000")
S3_ACCESS_KEY = os.environ.get("S3_ACCESS_KEY", "idre")
S3_SECRET_KEY = os.environ.get("S3_SECRET_KEY", "idre-secret")

SCHEMA = pa.schema([
    ("provider_id", pa.string()),
    ("tenant", pa.string()),
    ("entity_type", pa.string()),
    ("name", pa.string()),
    ("npi", pa.string()),
    ("tin", pa.string()),
    ("status", pa.string()),
    ("lat", pa.float64()),
    ("lon", pa.float64()),
    ("in_network", pa.bool_()),
])


def export_providers(tenant: str) -> int:
    import psycopg

    with psycopg.connect(DSN) as conn:
        rows = conn.execute(
            """SELECT id::text, tenant, entity_type, canonical_name, npi, tin, status
               FROM public.entities
               WHERE tenant = %s AND entity_type IN ('PROVIDER', 'FACILITY')
               ORDER BY id""",
            (tenant,),
        ).fetchall()

    cols = {name: [] for name in SCHEMA.names}
    for pid, t, etype, name, npi, tin, status in rows:
        cols["provider_id"].append(pid)
        cols["tenant"].append(t)
        cols["entity_type"].append(etype)
        cols["name"].append(name)
        cols["npi"].append(npi)
        cols["tin"].append(tin)
        cols["status"].append(status)
        cols["lat"].append(None)   # no service-location capture yet
        cols["lon"].append(None)
        cols["in_network"].append(None)
    table = pa.table(cols, schema=SCHEMA)

    s3 = pafs.S3FileSystem(access_key=S3_ACCESS_KEY, secret_key=S3_SECRET_KEY,
                           endpoint_override=S3_ENDPOINT, scheme="http",
                           region="us-east-1")
    path = f"{S3_BUCKET}/silver/providers/tenant={tenant}/providers.parquet"
    with s3.open_output_stream(path) as f:
        pq.write_table(table, f)
    print(f"pg_export: {len(rows)} providers -> {path}")
    return len(rows)


if __name__ == "__main__":
    export_providers(sys.argv[1])
