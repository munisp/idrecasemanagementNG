# Author: Patrick Munis at newwave
"""EPR-KGQA — Entity linking, Path Retrieval, KG-grounded Question Answering.

Pipeline for a natural-language question over a tenant's dispute graph:

    1. Entity linking   — case numbers / party names mentioned in the question
                          are resolved to graph nodes (graphdb.find_entities).
    2. Path retrieval   — shortest paths between mentioned entities, or the
                          2-hop neighborhood for a single entity. Paths are
                          the evidence and become the citations.
    3. GNN ranking      — when the question names a case, link-prediction
                          scores rank the related cases surfaced alongside.
    4. Answer           — ollama (local LLM, no PHI leaves the box) composes
                          the answer from the retrieved paths only. When no
                          ollama server is reachable the deterministic
                          extractive composer answers from the same paths and
                          the response is labeled generator=extractive-fallback.
    5. Logging          — every interaction is appended to the gold-zone
                          kgqa_logs parquet in ART schema (prompt, retrieved
                          context, completion, rating) — no training loop here,
                          the logs are the ART handoff.

Positive feedback reinforces the retrieved edges in FalkorDB, which the next
GNN training round consumes — closing the kgqa -> gnn loop.
"""

from __future__ import annotations

import json
import os
import re
import time

import httpx

import gnn
import graphdb
import lakehouse_io

OLLAMA_URL = os.environ.get("OLLAMA_URL", "http://localhost:11434")
OLLAMA_MODEL = os.environ.get("OLLAMA_MODEL", "qwen2.5:3b")
OLLAMA_TIMEOUT = float(os.environ.get("OLLAMA_TIMEOUT", "60"))

_WORD = re.compile(r"[A-Za-z0-9][A-Za-z0-9._\-]{2,}")
_STOP = {
    "the", "and", "for", "with", "from", "that", "this", "what", "which", "how",
    "many", "much", "are", "was", "were", "did", "does", "show", "list", "tell",
    "about", "between", "related", "cases", "case", "dispute", "disputes",
}


def _terms(question: str) -> list[str]:
    out, seen = [], set()
    for tok in _WORD.findall(question):
        t = tok.strip(".-").lower()
        if len(t) >= 3 and t not in _STOP and t not in seen:
            seen.add(t)
            out.append(tok.strip(".-"))
    return out[:6]


def link_entities(tenant: str, question: str) -> list[dict]:
    ents: list[dict] = []
    seen: set[str] = set()
    for term in _terms(question):
        for e in graphdb.find_entities(tenant, term):
            key = f"{e['kind']}:{e['id']}"
            if key not in seen:
                seen.add(key)
                ents.append(e)
        if len(ents) >= 8:
            break
    return ents


def _ollama_generate(prompt: str) -> tuple[str, str]:
    """Returns (text, generator_label). Raises on connectivity failure."""
    resp = httpx.post(
        f"{OLLAMA_URL}/api/generate",
        json={"model": OLLAMA_MODEL, "prompt": prompt, "stream": False,
              "options": {"temperature": 0.1}},
        timeout=OLLAMA_TIMEOUT,
    )
    resp.raise_for_status()
    return resp.json()["response"].strip(), f"ollama:{OLLAMA_MODEL}"


def _extractive_answer(question: str, entities: list[dict], paths: list[str],
                       ranked: list[dict]) -> str:
    lines = ["Based on the dispute graph (deterministic composer — ollama unreachable):"]
    if entities:
        ent_s = "; ".join(f"{e['label']} ({e['kind']}{', ' + e['detail'] if e['detail'] else ''})"
                          for e in entities[:5])
        lines.append(f"Entities matched: {ent_s}.")
    if paths:
        lines.append(f"{len(paths)} evidence path(s) connect these entities; the strongest are cited below.")
    if ranked:
        top = ", ".join(f"{r['case_id']} (score {r['score']})" for r in ranked[:3])
        lines.append(f"GNN link prediction ranks these as most related: {top}.")
    if not (entities or paths):
        lines.append("No matching cases or parties were found in this tenant's graph.")
    return "\n".join(lines)


def _prompt(question: str, paths: list[str]) -> str:
    evidence = "\n".join(f"- {p}" for p in paths[:12]) or "(no graph paths retrieved)"
    return (
        "You are the IDRE dispute-graph analyst for a federal No Surprises Act "
        "platform. Answer ONLY from the evidence paths below — every claim must "
        "trace to a path. If the evidence is insufficient, say so.\n\n"
        f"Evidence paths:\n{evidence}\n\nQuestion: {question}\nAnswer (concise, cite paths):"
    )


def ask(tenant: str, question: str, k: int = 5) -> dict:
    t0 = time.time()
    entities = link_entities(tenant, question)
    entity_ids = [e["id"] for e in entities]
    paths = graphdb.retrieve_paths(tenant, entity_ids) if entity_ids else []

    # GNN ranking for the first linked case (graph <-> gnn <-> kgqa)
    ranked: list[dict] = []
    case_ents = [e for e in entities if e["kind"] == "Case"]
    if case_ents:
        pred = gnn.predict(tenant, case_ents[0]["id"], k=k, write_back=False)
        ranked = pred.get("predictions", [])

    generator = ""
    try:
        answer, generator = _ollama_generate(_prompt(question, paths))
    except Exception:
        answer = _extractive_answer(question, entities, paths, ranked)
        generator = "extractive-fallback"

    log_id = lakehouse_io.append_kgqa_log({
        "tenant": tenant,
        "ts": int(time.time()),
        "question": question,
        "entities_json": json.dumps(entities),
        "retrieved_paths_json": json.dumps(paths),
        "answer": answer,
        "generator": generator,
        "gnn_ranked_json": json.dumps(ranked),
    })

    return {
        "log_id": log_id,
        "tenant": tenant,
        "question": question,
        "answer": answer,
        "entities": entities,
        "citations": [{"path": p} for p in paths],
        "gnn_ranked": ranked,
        "generator": generator,
        "latency_ms": int((time.time() - t0) * 1000),
    }


def feedback(tenant: str, log_id: str, rating: int) -> dict:
    """Thumbs up/down. Positive ratings reinforce the retrieved edges so the
    next GNN training round weights them higher (kgqa -> gnn)."""
    rating = 1 if rating > 0 else 0
    lakehouse_io.rate_kgqa_log(log_id, rating)
    reinforced = 0
    if rating == 1:
        for row in lakehouse_io.read_kgqa_logs(limit=2000):
            if row["log_id"] != log_id:
                continue
            for p in json.loads(row["retrieved_paths_json"] or "[]"):
                # paths render case nodes by case_number (or UUID) — accept both
                ids = re.findall(
                    r"\b(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
                    r"|[A-Z]{2,5}-[A-Z]{2}-\d{4}-\d+)\b", p)
                for a, b in zip(ids, ids[1:]):
                    reinforced += graphdb.reinforce_edge(tenant, a, b, 0.05, "kgqa")
            break
    return {"log_id": log_id, "rating": rating, "edges_reinforced": reinforced}
