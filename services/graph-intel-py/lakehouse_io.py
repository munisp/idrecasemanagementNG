# Author: Patrick Munis at newwave
"""Lakehouse IO — bidirectional bridge between the dispute graph and the
Parquet lakehouse (bronze/silver/gold zones under LAKEHOUSE_DIR).

    bronze/   raw append-only JSONL segments (case snapshots, graph events)
    silver/   normalized per-tenant case tables (cases_<tenant>.parquet)
    gold/     derived artifacts: graph exports, GNN link predictions,
              KGQA interaction logs (ART-ready)

The graph service is the only writer to gold/kgqa_logs — the analytics
service reads it, and a future ART training run consumes it directly
(prompt / retrieved_context / completion / rating columns).
"""

from __future__ import annotations

import json
import os
import time
import uuid
from pathlib import Path

import pyarrow as pa
import pyarrow.parquet as pq

LAKEHOUSE_DIR = Path(os.environ.get("LAKEHOUSE_DIR", "./lakehouse"))

CASE_SCHEMA = pa.schema(
    [
        ("id", pa.string()),
        ("case_number", pa.string()),
        ("status", pa.string()),
        ("service_line", pa.string()),
        ("plan_type", pa.string()),
        ("qpa_cents", pa.int64()),
        ("provider_id", pa.string()),
        ("payer_id", pa.string()),
        ("opened_at", pa.string()),
        ("synced_at", pa.int64()),
    ]
)

PREDICTION_SCHEMA = pa.schema(
    [
        ("tenant", pa.string()),
        ("src_case", pa.string()),
        ("dst_case", pa.string()),
        ("score", pa.float64()),
        ("model_version", pa.string()),
        ("predicted_at", pa.int64()),
    ]
)

KGQA_LOG_SCHEMA = pa.schema(
    [
        ("log_id", pa.string()),
        ("tenant", pa.string()),
        ("ts", pa.int64()),
        ("question", pa.string()),
        ("entities_json", pa.string()),
        ("retrieved_paths_json", pa.string()),
        ("answer", pa.string()),
        ("generator", pa.string()),  # ollama:<model> | extractive-fallback
        ("gnn_ranked_json", pa.string()),
        ("rating", pa.int64()),  # -1 unset, 0 negative, 1 positive
        ("rated_at", pa.int64()),
    ]
)


def _zone(name: str) -> Path:
    p = LAKEHOUSE_DIR / name
    p.mkdir(parents=True, exist_ok=True)
    return p


# ---------------- bronze ----------------

def append_bronze(segment: str, records: list[dict]) -> str:
    """Append a raw JSONL segment; returns the segment file path."""
    path = _zone("bronze") / f"{segment}-{int(time.time())}-{uuid.uuid4().hex[:8]}.jsonl"
    with path.open("w", encoding="utf-8") as f:
        for r in records:
            f.write(json.dumps(r, default=str) + "\n")
    return str(path)


# ---------------- silver ----------------

def write_silver_cases(tenant: str, rows: list[dict]) -> str:
    """Overwrite the tenant's normalized case table (small dims; full refresh)."""
    now = int(time.time())
    recs = [
        {
            "id": r["id"],
            "case_number": r.get("case_number") or "",
            "status": r.get("status") or "",
            "service_line": r.get("service_line") or "",
            "plan_type": r.get("plan_type") or "",
            "qpa_cents": int(r.get("qpa_cents") or 0),
            "provider_id": r.get("provider_id") or "",
            "payer_id": r.get("payer_id") or "",
            "opened_at": str(r.get("opened_at") or ""),
            "synced_at": now,
        }
        for r in rows
    ]
    table = pa.Table.from_pylist(recs, schema=CASE_SCHEMA) if recs else CASE_SCHEMA.empty_table()
    path = _zone("silver") / f"cases_{tenant}.parquet"
    pq.write_table(table, path)
    return str(path)


def read_silver_cases(tenant: str) -> list[dict]:
    path = _zone("silver") / f"cases_{tenant}.parquet"
    if not path.exists():
        return []
    return pq.read_table(path).to_pylist()


# ---------------- gold ----------------

def write_gold_graph_export(tenant: str, dump: dict) -> str:
    path = _zone("gold") / f"graph_export_{tenant}-{int(time.time())}.json"
    path.write_text(json.dumps(dump, default=str, indent=1), encoding="utf-8")
    return str(path)


def append_predictions(rows: list[dict]) -> str:
    return _append_gold("gnn_predictions", rows, PREDICTION_SCHEMA)


def append_kgqa_log(row: dict) -> str:
    row = dict(row)
    row.setdefault("log_id", uuid.uuid4().hex)
    row.setdefault("rating", -1)
    row.setdefault("rated_at", 0)
    _append_gold("kgqa_logs", [row], KGQA_LOG_SCHEMA)
    return row["log_id"]


def rate_kgqa_log(log_id: str, rating: int) -> bool:
    """Record thumbs up/down against a logged interaction. Parquet is
    immutable, so feedback lands as a compacted rating sidecar that the ART
    consumer joins on log_id."""
    sidecar = _zone("gold") / "kgqa_ratings.jsonl"
    with sidecar.open("a", encoding="utf-8") as f:
        f.write(json.dumps({"log_id": log_id, "rating": rating, "rated_at": int(time.time())}) + "\n")
    return True


def read_kgqa_logs(limit: int = 500) -> list[dict]:
    out: list[dict] = []
    for f in sorted((_zone("gold")).glob("kgqa_logs-*.parquet")):
        out.extend(pq.read_table(f).to_pylist())
    return out[-limit:]


def _append_gold(prefix: str, rows: list[dict], schema: pa.Schema) -> str:
    table = pa.Table.from_pylist(rows, schema=schema)
    path = _zone("gold") / f"{prefix}-{int(time.time())}-{uuid.uuid4().hex[:8]}.parquet"
    pq.write_table(table, path)
    return str(path)
