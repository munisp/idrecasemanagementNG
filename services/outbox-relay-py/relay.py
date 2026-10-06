"""Transactional outbox relay — the bridge between Postgres and Kafka.

case-api writes domain events to tenant_<st>.outbox inside the same transaction
as the state change (atomicity guarantee). This relay polls every tenant schema,
publishes unpublished rows to the matching Kafka topic, and marks them
published — at-least-once delivery, idempotent consumers downstream.

Without this service the event backbone is silent: doc-intel never receives
doc.uploaded, audit events never reach OpenSearch, and the lakehouse bronze
zone never sees case events.
"""

from __future__ import annotations

import json
import os
import time

import psycopg
from confluent_kafka import Producer

DSN = os.environ.get("PG_DSN", "postgresql://idre:idre@localhost:5432/idre")
BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092")
POLL_SECONDS = float(os.environ.get("RELAY_POLL_SECONDS", "0.5"))
BATCH = int(os.environ.get("RELAY_BATCH", "1000"))  # bigger claims per poll cycle

STATES = [s.strip() for s in os.environ.get("TENANTS", "").split(",") if s.strip()]


def tenant_schemas(conn) -> list[str]:
    if STATES:
        return [f"tenant_{s}" for s in STATES]
    rows = conn.execute(
        "SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant\\_\\_%'"
    ).fetchall()
    return [r[0] for r in rows]


def relay_once(conn, producer: Producer, schema: str) -> int:
    # Claim a batch atomically; SKIP LOCKED lets multiple replicas run safely.
    with conn.transaction():
        rows = conn.execute(
            f"""SELECT id, topic, key, payload FROM {schema}.outbox
                WHERE published_at IS NULL
                ORDER BY id LIMIT {BATCH} FOR UPDATE SKIP LOCKED"""
        ).fetchall()
        for _id, topic, key, payload in rows:
            producer.produce(
                topic,
                key=key.encode(),
                value=json.dumps(payload).encode(),
                headers={"source": "outbox-relay", "schema": schema},
            )
        producer.flush(timeout=10)
        ids = [r[0] for r in rows]
        if ids:
            conn.execute(
                f"UPDATE {schema}.outbox SET published_at=now() WHERE id = ANY(%s)", (ids,)
            )
    return len(rows)


def main() -> None:
    producer = Producer({
        "bootstrap.servers": BROKERS,
        "enable.idempotence": True,   # exactly-once on the Kafka side
        "acks": "all",
        # Throughput: micro-batching + lz4. Idempotence keeps ordering/dupes
        # safe while the linger window fills batches (~5 ms added latency).
        "linger.ms": "5",
        "batch.num.messages": "10000",
        "compression.type": "lz4",
        "queue.buffering.max.messages": "200000",
        "queue.buffering.max.kbytes": "1048576",   # 1 GiB in-flight cap
        "max.in.flight.requests.per.connection": "5",  # idempotence max
    })
    print(f"outbox-relay: polling {DSN} -> {BROKERS}", flush=True)
    while True:
        published = 0
        try:
            with psycopg.connect(DSN, autocommit=False) as conn:
                for schema in tenant_schemas(conn):
                    try:
                        published += relay_once(conn, producer, schema)
                    except psycopg.errors.UndefinedTable:
                        continue  # tenant not yet migrated
        except psycopg.OperationalError as e:
            print(f"outbox-relay: postgres unavailable: {e}", flush=True)
        time.sleep(POLL_SECONDS if published == 0 else 0.05)


if __name__ == "__main__":
    main()
