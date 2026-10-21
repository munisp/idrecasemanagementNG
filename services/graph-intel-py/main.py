"""graph-intel service — FalkorDB dispute graph, numpy GraphSAGE link
prediction, EPR-KGQA over ollama, and bidirectional lakehouse sync.

Endpoints (all tenant-scoped; the Go case-api proxies these with auth):
    GET  /healthz
    POST /sync/from-db?tenant=tx          Postgres -> FalkorDB -> silver parquet
    POST /sync/from-lakehouse?tenant=tx   silver parquet -> FalkorDB
    POST /sync/to-lakehouse?tenant=tx     FalkorDB -> gold parquet export
    POST /gnn/train?tenant=tx
    GET  /gnn/predict?tenant=tx&case_id=..&k=5
    GET  /graph/neighbors?tenant=tx&case_id=..&hops=2
    POST /ask        {tenant, question}   EPR-KGQA with citations
    POST /feedback   {tenant, log_id, rating}

Run: uvicorn main:app --host 0.0.0.0 --port 8082
"""

from __future__ import annotations

import os
import time

import psycopg
from fastapi import FastAPI, Query
from pydantic import BaseModel

import epr_kgqa
import gnn
import graphdb
import lakehouse_io

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")

# Lakehouse de-identification (deid-py): when DEID_URL is set, text fields
# are scrubbed BEFORE bronze/silver parquet writes and the audit report is
# posted back to case-api — the analytics tier stays PHI-free by
# construction. Unset = legacy behavior (pre-deid deployments). A deid
# outage fails the sync: unscrubbed PHI in the lakehouse is worse than a
# delayed sync.
DEID_URL = os.environ.get("DEID_URL", "")
CASE_API = os.environ.get("CASE_API_URL", "http://localhost:8080")
WORKER_TOKEN = os.environ.get("WORKER_TOKEN", "")


def _deid_rows(tenant: str, rows: list[dict]) -> list[dict]:
    """Scrub free-text fields through deid-py; report the decision."""
    if not DEID_URL:
        return rows
    import hashlib
    import httpx
    fields = ["provider_name", "payer_name", "provider_org", "health_plan",
              "requester_org", "service_line"]
    findings_total, detectors, policy = 0, [], "safe-harbor-18/v1"
    out = []
    joined = []
    for d in rows:
        d = dict(d)
        for f in fields:
            v = d.get(f)
            if v:
                joined.append(v)
        out.append(d)
    blob = "\n".join(joined)
    if blob.strip():
        resp = httpx.post(f"{DEID_URL}/v1/deidentify", timeout=60, json={
            "tenant": tenant, "artifact": "lakehouse", "text": blob})
        resp.raise_for_status()
        body = resp.json()
        redacted = body["text"].split("\n")
        audit = body["audit"]
        findings_total = sum(audit.get("counts", {}).values())
        detectors = audit.get("detectors", [])
        policy = audit.get("policy", policy)
        it = iter(redacted)
        for d in out:
            for f in fields:
                if d.get(f):
                    d[f] = next(it, d[f])
        # One report per sync run: input hash covers the joined blob.
        try:
            httpx.post(
                f"{CASE_API}/v1/tenants/{tenant}/internal/deid/reports",
                headers={"Authorization": f"Bearer {WORKER_TOKEN}"}, timeout=10,
                json={"artifact": "lakehouse",
                      "ref_id": hashlib.sha256(blob.encode()).hexdigest()[:16],
                      "action": "DEIDENTIFY", "audit": audit})
        except httpx.HTTPError:
            pass  # report persistence is best-effort; the scrub itself is not
    if findings_total:
        print(f"lakehouse deid: {findings_total} identifier(s) scrubbed "
              f"for {tenant} ({', '.join(detectors)}, {policy})", flush=True)
    return out

app = FastAPI(title="idre-graph-intel", version="1.0.0")
otel_setup.init("idre-graph-intel")
otel_setup.instrument_fastapi(app)


def _load_cases(tenant: str) -> list[dict]:
    t = "".join(c for c in tenant.lower() if c.isalnum())
    cols = ["id", "case_number", "status", "service_line", "plan_type",
            "qpa_cents", "provider_id", "payer_id", "opened_at"]
    # The parties as people know them. A program tenant records the filer's
    # organization and, once read from the documents, the provider's and the
    # plan's names; provider_id/payer_id there are internal ids nobody can
    # ask about by name. Names are used where they exist, ids otherwise.
    named = cols + ["provider_org", "health_plan", "requester_org", "filing_party_type"]
    with psycopg.connect(DSN, autocommit=True) as c:
        try:
            rows = c.execute(
                f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
                f" provider_id, payer_id, opened_at, details->>'provider_org', details->>'health_plan',"
                f" details->>'requester_org', details->>'filing_party_type' FROM tenant_{t}.cases"
            ).fetchall()
            use = named
        except Exception:  # a tenant whose cases table has no details column
            rows = c.execute(
                f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
                f" provider_id, payer_id, opened_at FROM tenant_{t}.cases"
            ).fetchall()
            use = cols
    out = []
    for r in rows:
        d = dict(zip(use, (str(v) if v is not None else None for v in r)))
        filer, by_plan = (d.get("requester_org") or "").strip(), d.get("filing_party_type") == "HEALTH_PLAN"
        d["provider_name"] = (d.get("provider_org") or "").strip() or ("" if by_plan else filer) or d.get("provider_id")
        d["payer_name"] = (d.get("health_plan") or "").strip() or (filer if by_plan else "") or d.get("payer_id")
        out.append(d)
    return out




app = FastAPI(title="idre-graph-intel", version="1.0.0")


def _load_cases(tenant: str) -> list[dict]:
    t = "".join(c for c in tenant.lower() if c.isalnum())
    with psycopg.connect(DSN) as c:
        rows = c.execute(
            f"SELECT id, case_number, status, service_line, plan_type, qpa_cents,"
            f" provider_id, payer_id, opened_at FROM tenant_{t}.cases"
        ).fetchall()
    cols = ["id", "case_number", "status", "service_line", "plan_type",
            "qpa_cents", "provider_id", "payer_id", "opened_at"]
    return [dict(zip(cols, (str(v) if v is not None else None for v in r))) for r in rows]


@app.get("/healthz")
def healthz():
    try:
        graphdb.q("tx", "RETURN 1")
        graph_ok = True
    except Exception:
        graph_ok = False
    return {"ok": True, "falkordb": graph_ok, "model": epr_kgqa.OLLAMA_MODEL}


@app.post("/sync/from-db")
def sync_from_db(tenant: str = Query(...)):
    """Source-of-truth sync: Postgres tenant schema -> FalkorDB + bronze/silver."""
    cases = _load_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    # FalkorDB gets the operational graph unscrubbed (it is a transactional
    # system, tenant-isolated); the lakehouse tiers get the de-identified
    # projection — analytics must never hold raw PHI.
    scrubbed = _deid_rows(tenant, cases)
    bronze = lakehouse_io.append_bronze(f"cases-{tenant}", scrubbed)
    silver = lakehouse_io.write_silver_cases(tenant, scrubbed)
    return {"tenant": tenant, "cases": len(cases), "bronze": bronze, "silver": silver}


@app.post("/sync/from-lakehouse")
def sync_from_lakehouse(tenant: str = Query(...)):
    """Lakehouse -> graph: rebuild FalkorDB from the silver zone."""
    cases = lakehouse_io.read_silver_cases(tenant)
    for c in cases:
        graphdb.upsert_case(tenant, c)
    return {"tenant": tenant, "restored": len(cases)}


@app.post("/sync/to-lakehouse")
def sync_to_lakehouse(tenant: str = Query(...)):
    """Graph -> lakehouse: export nodes/edges (incl. GNN/KGQA-written edges)
    to the gold zone."""
    dump = graphdb.dump_graph(tenant)
    path = lakehouse_io.write_gold_graph_export(tenant, dump)
    return {"tenant": tenant, "export": path,
            "cases": len(dump["cases"]), "case_edges": len(dump["case_edges"])}


@app.post("/gnn/train")
def gnn_train(tenant: str = Query(...), epochs: int = gnn.EPOCHS):
    return gnn.train(tenant, epochs=epochs)


@app.get("/gnn/predict")
def gnn_predict(tenant: str = Query(...), case_id: str = Query(...), k: int = 5):
    return gnn.predict(tenant, case_id, k=k)


@app.get("/graph/neighbors")
def neighbors(tenant: str = Query(...), case_id: str = Query(...), hops: int = 2):
    return graphdb.neighbors(tenant, case_id, hops=hops)


class AskRequest(BaseModel):
    tenant: str
    question: str
    k: int = 5


@app.post("/ask")
def ask(req: AskRequest):
    return epr_kgqa.ask(req.tenant, req.question, k=req.k)


class FeedbackRequest(BaseModel):
    tenant: str
    log_id: str
    rating: int  # 1 up, 0 down


@app.post("/feedback")
def feedback(req: FeedbackRequest):
    return epr_kgqa.feedback(req.tenant, req.log_id, req.rating)


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=int(os.environ.get("PORT", "8082")))
