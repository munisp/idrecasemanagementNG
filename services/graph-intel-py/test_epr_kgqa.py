"""KGQA correctness tests — the pipeline must return grounded answers:
entities actually mentioned in the question, paths only from the graph,
extractive fallback when ollama is down, and a log entry every time."""
import sys, types
from unittest import mock

# Stub infra modules before importing epr_kgqa (no FalkorDB/ollama in CI).
graphdb = types.ModuleType("graphdb")
gnn = types.ModuleType("gnn")
lake = types.ModuleType("lakehouse_io")
sys.modules["graphdb"] = graphdb
sys.modules["gnn"] = gnn
sys.modules["lakehouse_io"] = lake

CASE = {"id": "c-1", "kind": "Case", "label": "IDR-2026-0004", "detail": "OPEN"}
PAYER = {"id": "p-9", "kind": "Payer", "label": "Acme Health", "detail": ""}
PATHS = ["(IDR-2026-0004)-[:AGAINST]->(Acme Health)<-[:AGAINST]-(IDR-2026-0007)"]

import epr_kgqa as k  # noqa: E402


def _wire():
    graphdb.find_entities = lambda tenant, term: (
        [CASE] if "idr-2026-0004" in term.lower() else
        [PAYER] if "acme" in term.lower() else [])
    graphdb.retrieve_paths = lambda tenant, ids: PATHS if ids else []
    gnn.predict = lambda tenant, cid, k=5, write_back=False: {
        "predictions": [{"case_id": "c-7", "score": 0.91}]}
    lake.append_kgqa_log = lambda rec: "log-123"


def test_terms_keep_case_numbers_and_drop_stopwords():
    ts = [t.lower() for t in k._terms("Which disputes share a payer with case IDR-2026-0004?")]
    assert "idr-2026-0004" in ts and "which" not in ts and "case" not in ts


def test_ask_extractive_fallback_cites_paths_and_entities():
    _wire()
    with mock.patch.object(k, "_ollama_generate", side_effect=Exception("down")):
        r = k.ask("t1", "Which disputes share a payer with case IDR-2026-0004?")
    assert r["generator"] == "extractive-fallback"
    assert r["citations"] == PATHS or any(p in str(r.get("citations")) for p in PATHS)
    labels = [e["label"] for e in r["entities"]]
    assert "IDR-2026-0004" in labels
    assert r["gnn_ranked"] and r["gnn_ranked"][0]["case_id"] == "c-7"
    assert r["log_id"] == "log-123"


def test_ask_uses_ollama_when_reachable():
    _wire()
    with mock.patch.object(k, "_ollama_generate", return_value=("grounded answer", "ollama:test")):
        r = k.ask("t1", "Tell me about IDR-2026-0004")
    assert r["generator"] == "ollama:test" and r["answer"] == "grounded answer"


def test_ask_no_entities_is_honest():
    _wire()
    with mock.patch.object(k, "_ollama_generate", side_effect=Exception("down")):
        r = k.ask("t1", "anything about zzz-unknown")
    assert r["entities"] == [] and r["citations"] == []
    assert "No matching" in r["answer"]
