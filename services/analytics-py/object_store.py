# Author: Patrick Munis at newwave
"""Analytics object I/O over Apache OpenDAL (pip package `opendal`).

One operator configured from env; every raw-object read/write in the
analytics tier goes through it. Table-format engines (Spark s3a, Delta
transaction logs, DataFusion registration) keep their own filesystem
bindings — that's the documented boundary: OpenDAL is the application
object layer, not a replacement for delta-rs/Hadoop filesystems inside
table engines.

Env: OPENDAL_SCHEME (default s3), OPENDAL_BUCKET (default lakehouse),
OPENDAL_ENDPOINT, OPENDAL_REGION, OPENDAL_ACCESS_KEY_ID,
OPENDAL_SECRET_ACCESS_KEY, OPENDAL_ROOT (default "/").
"""

from __future__ import annotations

import os

import opendal

_SCHEME = os.environ.get("OPENDAL_SCHEME", "s3")


def get_operator() -> opendal.Operator:
    """Build the tier-wide operator from env. Retry layer matches the
    gateway's discipline: transient blips must not fail batch jobs."""
    kwargs: dict = {"root": os.environ.get("OPENDAL_ROOT", "/")}
    if _SCHEME == "s3":
        kwargs.update(
            bucket=os.environ.get("OPENDAL_BUCKET", "lakehouse"),
            endpoint=os.environ.get("OPENDAL_ENDPOINT", "http://minio:9000"),
            region=os.environ.get("OPENDAL_REGION", "us-east-1"),
            access_key_id=os.environ.get("OPENDAL_ACCESS_KEY_ID", "idre"),
            secret_access_key=os.environ.get("OPENDAL_SECRET_ACCESS_KEY", "idre-secret"),
        )
    op = opendal.Operator(_SCHEME, **kwargs)
    return op.layer(opendal.layers.RetryLayer(max_times=3))


def write_bytes(op: opendal.Operator, path: str, data: bytes) -> None:
    op.write(path, data)


def read_bytes(op: opendal.Operator, path: str) -> bytes | None:
    """None when the object does not exist (model-not-trained-yet is a
    normal state, not an exception worth handling)."""
    try:
        return bytes(op.read(path))
    except opendal.exceptions.NotFoundError:
        return None


def write_parquet(op: opendal.Operator, path: str, table) -> None:
    """Serialize a pyarrow Table to parquet bytes, then one opendal write —
    pyarrow.fs is bypassed so the object layer stays OpenDAL-only."""
    import pyarrow.parquet as pq

    sink = __import__("pyarrow").BufferOutputStream()
    pq.write_table(table, sink)
    op.write(path, sink.getvalue().to_pybytes())
