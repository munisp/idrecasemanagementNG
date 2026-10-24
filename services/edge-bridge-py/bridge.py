# Author: Patrick Munis at newwave
"""Fluvio edge bridge — edge ingestion into the Kafka backbone.

Voice platforms, state portals and field devices publish to Fluvio topics at
the edge (lightweight, runs anywhere). This bridge consumes those topics and
republishes each record onto the matching Kafka topic in the platform core,
preserving tenant routing: fluvio topic "idre-edge-tx-voice" becomes Kafka
topic "idre.tx.voice".

Runs with graceful degradation: if the fluvio client library or the edge
cluster is unavailable, the bridge idles and retries — the core platform is
never blocked by an edge outage.
"""

from __future__ import annotations

import json
import os
import time

KAFKA_BROKERS = os.environ.get("KAFKA_BROKERS", "localhost:9092")
FLUVIO_ENDPOINT = os.environ.get("FLUVIO_ENDPOINT", "")  # "" = edge disabled
TOPIC_MAP = {  # fluvio edge topic -> kafka core topic suffix
    "voice": "voice",
    "documents": "documents",
    "offers": "offers",
}


def kafka_producer():
    from confluent_kafka import Producer
    return Producer({"bootstrap.servers": KAFKA_BROKERS, "acks": "all"})


def main() -> None:
    if not FLUVIO_ENDPOINT:
        print("edge-bridge: FLUVIO_ENDPOINT unset — edge ingestion disabled, idling", flush=True)
        while True:
            time.sleep(3600)

    try:
        from fluvio import Fluvio  # optional dependency, edge-side only
    except ImportError:
        print("edge-bridge: fluvio client not installed — idling", flush=True)
        while True:
            time.sleep(3600)

    producer = kafka_producer()
    while True:
        try:
            fluvio = Fluvio.connect(FLUVIO_ENDPOINT)
            for edge_suffix, core_suffix in TOPIC_MAP.items():
                for tenant in os.environ.get("TENANTS", "").split(","):
                    tenant = tenant.strip()
                    if not tenant:
                        continue
                    topic = f"idre-edge-{tenant}-{edge_suffix}"
                    consumer = fluvio.partition_consumer(topic, 0)
                    for record in consumer.stream():
                        value = record.value()
                        producer.produce(
                            f"idre.{tenant}.{core_suffix}",
                            key=tenant.encode(),
                            value=value if isinstance(value, bytes) else json.dumps(value).encode(),
                            headers={"source": "fluvio-edge", "edge_topic": topic},
                        )
                    producer.flush()
        except Exception as e:  # edge outage must never crash the bridge loop
            print(f"edge-bridge: {e} — retrying in 10s", flush=True)
            time.sleep(10)


if __name__ == "__main__":
    main()
