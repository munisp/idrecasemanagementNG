# Author: Patrick Munis at newwave
"""FalkorDB graph access layer — the single graph store for the platform.

Graph model (one graph per tenant, name: idre_<tenant>):

    (:Case  {id, case_number, status, service_line, plan_type, qpa_cents, opened_at})
    (:Party {name, role})                      role in {provider, payer}
    (Case)-[:FILED_BY]->(Party{role:provider})
    (Case)-[:AGAINST]->(Party{role:payer})
    (Case)-[:RELATED_TO {source, score}]->(Case)   source in {gnn, kgqa, manual}
    (Case)-[:DUPLICATE_OF]->(Case)

All Cypher goes through the official `falkordb` client (RESP/GRAPH.QUERY).
Every write records provenance (source + timestamp) so the lakehouse sync can
round-trip graph state without guessing where a fact came from.
"""

from __future__ import annotations

import os
import time
from typing import Any

from falkordb import FalkorDB

FALKOR_HOST = os.environ.get("FALKORDB_HOST", "localhost")
FALKOR_PORT = int(os.environ.get("FALKORDB_PORT", "6397"))
FALKOR_SOCKET = os.environ.get("FALKORDB_SOCKET", "")  # embedded FalkorDB Lite

# Lazy, not constructed at import time: the FalkorDB client's __init__ makes a
# real connection and raises if it can't (reproduced locally: a plain `import
# graphdb` with FalkorDB unreachable crashes the whole process before FastAPI
# ever starts listening). That defeats /healthz's own design, which expects a
# FalkorDB outage to show up as {"falkordb": false}, not a crash -- and on a
# fresh deploy, graph-intel and FalkorDB starting in either order is normal,
# not a fault condition.
_db = None


def _client():
    global _db
    if _db is None:
        _db = (
            FalkorDB(unix_socket_path=FALKOR_SOCKET)
            if FALKOR_SOCKET
            else FalkorDB(host=FALKOR_HOST, port=FALKOR_PORT)
        )
    return _db


def graph_name(tenant: str) -> str:
    t = "".join(c for c in tenant.lower() if c.isalnum())
    return f"idre_{t}"


def g(tenant: str):
    return _client().select_graph(graph_name(tenant))


def q(tenant: str, cypher: str, params: dict[str, Any] | None = None):
    return g(tenant).query(cypher, params or {})


def upsert_case(tenant: str, c: dict) -> None:
    """Idempotent case + parties upsert. Safe to replay from any source."""
    q(
        tenant,
        """
        MERGE (c:Case {id: $id})
        SET c.case_number=$case_number, c.status=$status,
            c.service_line=$service_line, c.plan_type=$plan_type,
            c.qpa_cents=$qpa_cents, c.opened_at=$opened_at, c.synced_at=$ts
        WITH c
        MERGE (p:Party {name: $provider})
          ON CREATE SET p.role='provider'
        MERGE (c)-[:FILED_BY]->(p)
        WITH c
        MERGE (r:Party {name: $payer})
          ON CREATE SET r.role='payer'
        MERGE (c)-[:AGAINST]->(r)
        """,
        {
            "id": c["id"],
            "case_number": c.get("case_number") or "",
            "status": c.get("status") or "",
            "service_line": c.get("service_line") or "",
            "plan_type": c.get("plan_type") or "",
            "qpa_cents": int(c.get("qpa_cents") or 0),
            "opened_at": str(c.get("opened_at") or ""),
            "provider": c.get("provider_id") or "unknown-provider",
            "payer": c.get("payer_id") or "unknown-payer",
            "ts": int(time.time()),
        },
    )


def write_related(tenant: str, src: str, dst: str, score: float, source: str) -> None:
    """GNN / KGQA / manual link-prediction write-back (intelligence -> graph).
    Accepts case UUIDs or case numbers."""
    q(
        tenant,
        """
        MATCH (a:Case), (b:Case)
        WHERE (a.id = $src OR a.case_number = $src)
          AND (b.id = $dst OR b.case_number = $dst)
        MERGE (a)-[r:RELATED_TO]->(b)
        SET r.score=$score, r.source=$source, r.updated_at=$ts
        """,
        {"src": src, "dst": dst, "score": float(score), "source": source, "ts": int(time.time())},
    )


def reinforce_edge(tenant: str, src: str, dst: str, delta: float, source: str) -> int:
    """KGQA feedback loop: positively-rated retrieval paths strengthen the edge
    the GNN will see next training round (kgqa -> gnn direction).
    Returns the number of edges updated."""
    res = q(
        tenant,
        """
        MATCH (a:Case)-[r:RELATED_TO]->(b:Case)
        WHERE (a.id = $src OR a.case_number = $src)
          AND (b.id = $dst OR b.case_number = $dst)
        SET r.score = coalesce(r.score, 0.5) + $delta,
            r.source = $source, r.updated_at = $ts
        RETURN count(r) AS n
        """,
        {"src": src, "dst": dst, "delta": float(delta), "source": source, "ts": int(time.time())},
    )
    n = int(res.result_set[0][0]) if res.result_set else 0
    if n == 0:
        # no direct edge yet (path ran through a Party): lay a weak KGQA edge
        write_related(tenant, src, dst, 0.5 + delta, "kgqa")
        n = 1
    return n


def case_index(tenant: str) -> list[dict]:
    """All case nodes — GNN feature construction reads this (graph -> gnn)."""
    res = q(
        tenant,
        "MATCH (c:Case) RETURN c.id, c.case_number, c.status, c.service_line,"
        " c.plan_type, c.qpa_cents, c.opened_at",
    )
    cols = ["id", "case_number", "status", "service_line", "plan_type", "qpa_cents", "opened_at"]
    return [dict(zip(cols, row)) for row in res.result_set]


def case_edges(tenant: str) -> list[tuple[str, str, str, float]]:
    """All case-to-case edges as (src, dst, type, weight). Party-mediated
    connectivity (same provider/payer) is derived in gnn.py from node attrs."""
    res = q(
        tenant,
        """
        MATCH (a:Case)-[r]->(b:Case)
        RETURN a.id, b.id, type(r), coalesce(r.score, 1.0)
        """,
    )
    return [(r[0], r[1], r[2], float(r[3])) for r in res.result_set]


def party_edges(tenant: str) -> list[tuple[str, str, str]]:
    """(case_id, edge_type, party_name) for feature building and KGQA paths."""
    res = q(
        tenant,
        """
        MATCH (c:Case)-[r:FILED_BY|AGAINST]->(p:Party)
        RETURN c.id, type(r), p.name
        """,
    )
    return [(r[0], r[1], r[2]) for r in res.result_set]


def neighbors(tenant: str, case_id: str, hops: int = 2) -> dict:
    """Neighborhood used by the Go middleware /graph-neighbors endpoint."""
    hops = max(1, min(hops, 3))
    res = q(
        tenant,
        f"""
        MATCH (c:Case {{id: $id}})-[*1..{hops}]-(n)
        RETURN DISTINCT labels(n)[0] AS kind,
               coalesce(n.id, n.name) AS key,
               coalesce(n.case_number, n.name) AS label,
               coalesce(n.status, n.role, '') AS detail
        LIMIT 200
        """,
        {"id": case_id},
    )
    return {
        "case_id": case_id,
        "hops": hops,
        "nodes": [
            {"kind": r[0], "id": r[1], "label": r[2], "detail": r[3]}
            for r in res.result_set
        ],
    }


def find_entities(tenant: str, term: str) -> list[dict]:
    """Entity linking for EPR-KGQA: match a free-text term to case numbers
    (exact / prefix) or party names (case-insensitive substring)."""
    res = q(
        tenant,
        """
        MATCH (c:Case)
        WHERE toLower(c.case_number) CONTAINS toLower($term)
        RETURN 'Case' AS kind, c.id AS id, c.case_number AS label, c.status AS detail
        UNION
        MATCH (p:Party)
        WHERE toLower(p.name) CONTAINS toLower($term)
        RETURN 'Party' AS kind, p.name AS id, p.name AS label, p.role AS detail
        LIMIT 25
        """,
        {"term": term},
    )
    return [{"kind": r[0], "id": r[1], "label": r[2], "detail": r[3]} for r in res.result_set]


def retrieve_paths(tenant: str, entity_ids: list[str], limit: int = 12) -> list[str]:
    """Path retrieval: shortest paths between the mentioned entities, or the
    2-hop neighborhood when a single entity was mentioned. Returned as
    human-readable path strings — these become the KGQA citations."""
    paths: list[str] = []
    if len(entity_ids) >= 2:
        res = q(
            tenant,
            """
            MATCH (a), (b)
            WHERE (a:Case AND a.id = $src OR a:Party AND a.name = $src)
              AND (b:Case AND b.id = $dst OR b:Party AND b.name = $dst)
            MATCH p = (a)-[*1..4]-(b)
            RETURN [n IN nodes(p) | coalesce(n.case_number, n.name)] AS names,
                   [r IN relationships(p) | type(r)] AS rels,
                   length(p) AS plen
            ORDER BY plen
            LIMIT $limit
            """,
            {"src": entity_ids[0], "dst": entity_ids[1], "limit": limit},
        )
        for names, rels, _plen in res.result_set:
            paths.append(_render_path(names, rels))
    elif entity_ids:
        eid = entity_ids[0]
        res = q(
            tenant,
            """
            MATCH (e)
            WHERE e:Case AND e.id = $eid OR e:Party AND e.name = $eid
            MATCH p = (e)-[*1..2]-(n)
            RETURN [x IN nodes(p) | coalesce(x.case_number, x.name)] AS names,
                   [r IN relationships(p) | type(r)] AS rels
            LIMIT $limit
            """,
            {"eid": eid, "limit": limit},
        )
        for names, rels in res.result_set:
            paths.append(_render_path(names, rels))
    return paths


def _render_path(names: list[str], rels: list[str]) -> str:
    out = names[0] if names else ""
    for rel, nxt in zip(rels, names[1:]):
        out += f" -[{rel}]-> {nxt}"
    return out


def dump_graph(tenant: str) -> dict:
    """Full export for the graph -> lakehouse direction."""
    return {
        "tenant": tenant,
        "exported_at": int(time.time()),
        "cases": case_index(tenant),
        "case_edges": [
            {"src": s, "dst": d, "type": t, "weight": w}
            for s, d, t, w in case_edges(tenant)
        ],
        "party_edges": [
            {"case_id": c, "type": t, "party": p} for c, t, p in party_edges(tenant)
        ],
    }
