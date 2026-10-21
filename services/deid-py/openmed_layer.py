"""openmed_layer.py — optional OpenMed transformer layer.

The Safe Harbor regex floor (safeharbor.py) is deterministic but blind to
unlabeled free-text names and places ("Dr. Alvarez at Coral Gables"). OpenMed
adds that recall. Loading is explicit and local: models must be staged under
OPENMED_MODEL_DIR (air-gap friendly — the service never phones out). When the
SDK or the staged models are absent the service runs in regex-only mode and
says so in /healthz; nothing silently degrades.
"""

from __future__ import annotations

import os
from dataclasses import asdict

from safeharbor import Finding

MODEL_DIR = os.environ.get("OPENMED_MODEL_DIR", "/models")
PII_MODEL = os.environ.get("OPENMED_PII_MODEL", "OpenMed/PII-Detect-Small-434M-v2")
CLINICAL_MODEL = os.environ.get("OPENMED_CLINICAL_MODEL", "OpenMed/OpenMed-NER-PathologyDetect-SuperClinical-434M")

_pii = None
_clinical = None
_tried: dict[str, Exception | None] = {}


def _load(model: str):
    """Lazy-load one OpenMed pipeline from the local staging dir."""
    key = model
    if key in _tried:
        return _pii if model == PII_MODEL else _clinical
    try:
        from openmed import OpenMedPipeline  # type: ignore
        local = os.path.join(MODEL_DIR, model.replace("/", "__"))
        model_id = local if os.path.isdir(local) else model
        pipe = OpenMedPipeline(model_id)
        _tried[key] = None
        return pipe
    except Exception as exc:  # SDK absent, model not staged, torch missing…
        _tried[key] = exc
        return None


def status() -> dict:
    return {
        "pii": "loaded" if _tried.get(PII_MODEL) is None and PII_MODEL in _tried else (
            "unavailable: %s" % _tried[PII_MODEL] if PII_MODEL in _tried else "not attempted"),
        "clinical": "loaded" if _tried.get(CLINICAL_MODEL) is None and CLINICAL_MODEL in _tried else (
            "unavailable: %s" % _tried[CLINICAL_MODEL] if CLINICAL_MODEL in _tried else "not attempted"),
    }


def _entities(pipe, text: str, detector: str) -> list[Finding]:
    out = []
    for ent in pipe.predict(text):
        label = getattr(ent, "label", "") or ent.get("label", "")
        start = getattr(ent, "start", None) if not isinstance(ent, dict) else ent.get("start")
        end = getattr(ent, "end", None) if not isinstance(ent, dict) else ent.get("end")
        score = getattr(ent, "score", 0.8) if not isinstance(ent, dict) else ent.get("score", 0.8)
        surface = text[start:end] if isinstance(start, int) and isinstance(end, int) else str(getattr(ent, "text", "") or ent.get("text", ""))
        if surface:
            out.append(Finding(str(label).upper(), int(start or 0), int(end or 0), float(score), surface, detector))
    return out


def pii_scan(text: str) -> list[Finding]:
    """NER recall for names/places/etc. Empty when the layer is unavailable."""
    global _pii
    _pii = _load(PII_MODEL)
    if _pii is None:
        return []
    return _entities(_pii, text, "openmed-pii")


def clinical_scan(text: str) -> list[Finding]:
    global _clinical
    _clinical = _load(CLINICAL_MODEL)
    if _clinical is None:
        return []
    return _entities(_clinical, text, "openmed-clinical")
