"""deid service — PHI/PII de-identification + clinical NER for the platform.

Phase 1 (privacy): /v1/scan and /v1/deidentify sit in front of every outbound
artifact path (QA-approved correspondence, lakehouse sync) and answer two
questions: "what identifiers are in this text?" and "give me a Safe-Harbor
de-identified version". Every call returns a signed-shape audit report
(hash of input, policy version, detector versions, per-type counts) — the
caller persists it via case-api, so "what left the building, scrubbed how?"
is answerable from the record.

Phase 2 (clinical): /v1/clinical extracts drugs/conditions/anatomy/procedures
from document text for doc-intel (screening + duplicate signals).

The Safe Harbor regex layer is always on; OpenMed transformer models are an
optional recall layer staged under OPENMED_MODEL_DIR (air-gap friendly).
/v1/clinical works without them at lexicon recall and reports which
detectors ran — no silent degradation anywhere.

Run: uvicorn main:app --port 8083
"""

from __future__ import annotations

import os
import time
from typing import Optional

from fastapi import FastAPI
from pydantic import BaseModel, Field

import clinical
import openmed_layer
from safeharbor import POLICY_VERSION, redact, risk, scan, text_hash

app = FastAPI(title="deid", version="1.0.0")

SERVICE_VERSION = "1.0.0"


class TextReq(BaseModel):
    tenant: str
    text: str = Field(max_length=500_000)
    case_id: Optional[str] = None
    artifact: str = "unknown"          # letter|email|package|lakehouse|document
    shift_dates: bool = True


def _audit(req: TextReq, action: str, findings, detectors: list[str], out_text: str | None = None) -> dict:
    counts: dict[str, int] = {}
    for f in findings:
        counts[f.type] = counts.get(f.type, 0) + 1
    return {
        "tenant": req.tenant,
        "case_id": req.case_id,
        "artifact": req.artifact,
        "action": action,
        "policy": POLICY_VERSION,
        "detectors": detectors,
        "counts": counts,
        "findings": [f.to_dict() for f in findings],
        "risk": risk(findings),
        "text_hash": text_hash(req.text),               # input never stored
        "output_hash": text_hash(out_text) if out_text is not None else None,
        "service_version": SERVICE_VERSION,
        "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }


@app.get("/healthz")
def healthz():
    om = openmed_layer.status()
    return {"ok": True, "policy": POLICY_VERSION, "openmed": om,
            "mode": "regex+ner" if om["pii"] == "loaded" else "regex-only"}


@app.post("/v1/scan")
def v1_scan(req: TextReq):
    """What identifiers are in this text? (No modification.)"""
    findings = scan(req.text)
    detectors = ["safeharbor-regex/" + POLICY_VERSION]
    ner = openmed_layer.pii_scan(req.text)
    if ner:
        covered = [(f.start, f.end) for f in findings]
        ner = [f for f in ner if not any(s < f.end and f.start < e for (s, e) in covered)]
        findings = sorted(findings + ner, key=lambda f: f.start)
        detectors.append("openmed-pii")
    return {"findings": [f.to_dict() for f in findings], "risk": risk(findings),
            "audit": _audit(req, "SCAN", findings, detectors)}


@app.post("/v1/deidentify")
def v1_deidentify(req: TextReq):
    """Return the Safe-Harbor de-identified text (dates shifted, the rest
    token-replaced) plus the audit report."""
    findings = scan(req.text)
    detectors = ["safeharbor-regex/" + POLICY_VERSION]
    ner = openmed_layer.pii_scan(req.text)
    if ner:
        covered = [(f.start, f.end) for f in findings]
        ner = [f for f in ner if not any(s < f.end and f.start < e for (s, e) in covered)]
        findings = sorted(findings + ner, key=lambda f: f.start)
        detectors.append("openmed-pii")
    seed = os.environ.get("DEID_DATE_SHIFT_SEED", "deid-dev-seed") + ":" + req.tenant
    out_text = redact(req.text, findings, seed, shift_dates=req.shift_dates)
    return {"text": out_text,
            "audit": _audit(req, "DEIDENTIFY", findings, detectors, out_text)}


@app.post("/v1/clinical")
def v1_clinical(req: TextReq):
    """Clinical entity extraction (drug/condition/anatomy/procedure)."""
    ents, detectors = clinical.analyze(req.text)
    return {"entities": [e.to_dict() for e in ents],
            "summary": clinical.summarize(ents),
            "detectors": detectors,
            "audit": _audit(req, "CLINICAL", ents, detectors)}
