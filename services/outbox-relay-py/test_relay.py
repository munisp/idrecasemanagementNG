"""Unit tests for the transactional outbox relay.

The relay is the only bridge between Postgres and Kafka — a regression here
silently kills the event backbone. Fakes stand in for psycopg connections and
the Kafka producer so the claim/publish/mark-published contract is testable
without infrastructure.
"""

from __future__ import annotations

import json

import relay


class FakeCursor:
    def __init__(self, conn):
        self.conn = conn

    def fetchall(self):
        return self.conn.rows


class FakeConn:
    """Records SQL and serves scripted rows per relay_once cycle."""

    def __init__(self, rows):
        self.rows = rows
        self.queries: list[tuple[str, object]] = []
        self.committed = False

    def execute(self, sql, params=None):
        self.queries.append((sql, params))
        return FakeCursor(self)

    def transaction(self):
        return self

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        if exc[0] is None:
            self.committed = True
        return False


class FakeProducer:
    def __init__(self):
        self.messages = []
        self.flushed = False

    def produce(self, topic, key=None, value=None, headers=None):
        self.messages.append({"topic": topic, "key": key, "value": value, "headers": headers})

    def flush(self, timeout=0):
        self.flushed = True


def test_tenant_schemas_env_override(monkeypatch):
    monkeypatch.setattr(relay, "STATES", ["fl", "tx"])
    schemas = relay.tenant_schemas(FakeConn([]))
    assert schemas == ["tenant_fl", "tenant_tx"]


def test_tenant_schemas_discovered_from_catalog(monkeypatch):
    monkeypatch.setattr(relay, "STATES", [])
    conn = FakeConn([("tenant_fl",), ("tenant_tx",)])
    schemas = relay.tenant_schemas(conn)
    assert schemas == ["tenant_fl", "tenant_tx"]
    assert "information_schema.schemata" in conn.queries[0][0]


def test_relay_once_publishes_and_marks():
    rows = [
        (1, "case.events", "k1", {"case_id": "a"}),
        (2, "case.events", "k2", {"case_id": "b"}),
    ]
    conn = FakeConn(rows)
    producer = FakeProducer()
    n = relay.relay_once(conn, producer, "tenant_fl")
    assert n == 2
    assert producer.flushed
    assert [m["key"] for m in producer.messages] == [b"k1", b"k2"]
    assert json.loads(producer.messages[0]["value"]) == {"case_id": "a"}
    assert producer.messages[0]["headers"]["schema"] == "tenant_fl"
    # Claim uses SKIP LOCKED; published rows are marked in the same transaction.
    assert "FOR UPDATE SKIP LOCKED" in conn.queries[0][0]
    assert any("SET published_at=now()" in q for q, _ in conn.queries)
    assert conn.committed


def test_relay_once_empty_batch_still_flushes_but_no_update():
    conn = FakeConn([])
    producer = FakeProducer()
    n = relay.relay_once(conn, producer, "tenant_fl")
    assert n == 0
    assert producer.flushed
    assert not any("SET published_at" in q for q, _ in conn.queries)
