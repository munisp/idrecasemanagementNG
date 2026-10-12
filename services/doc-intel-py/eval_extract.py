"""Field-accuracy evaluation harness for the extraction pipeline.

Runs the deterministic extraction chain (dewatermark -> OCR -> typed-field
extraction) over labeled fixtures and scores field-level accuracy against
ground truth. This is the objective gate for every OCR/extraction change —
"the model is >95% accurate" is a measurement, not a vibe:

    python eval_extract.py [testdata/eval]            # report
    ACCURACY_MIN=0.95 python eval_extract.py          # gate (exit 1 below)

Fixtures: a directory of document images plus ground_truth.json mapping each
filename to the field values a human verified on the document. As reviewed
corrections accumulate (public.doc_feedback), new verified documents should
be exported here so the gate measures the document mix the platform actually
sees — this is also the champion/challenger corpus for learned patterns.

OCR engine note: the production primary engine is PaddleOCR; this harness
uses it when importable (set EVAL_OCR=tesseract to force the offline floor).
Tesseract scores strictly worse on the same degraded scans — with tesseract
the harness is the floor, not the ceiling.
"""

from __future__ import annotations

import json
import os
import re
import sys

import cv2

import dewatermark
import fields as _fields

# Memory guard for OCR containers: images above this many pixels are read in
# overlapping horizontal bands instead of whole-page, so a 2x-upscaled dense
# scan cannot OOM the worker (a 4 GB container fits ~2 MP per PaddleOCR call).
_OCR_MAX_PIXELS = 2_000_000
_OCR_BAND_OVERLAP = 80


def _enhance(gray):
    """Dewatermark, then denoise (fax grain survives lattice subtraction), then
    upscale small pages — recognition accuracy on dense EOB tables depends on
    glyph height, and 10 px glyphs are below every engine's floor."""
    enhanced, _removed = dewatermark.enhance_page(gray)
    enhanced = cv2.fastNlMeansDenoising(enhanced, None, 12, 7, 21)
    h, w = enhanced.shape
    if max(h, w) < 2000:
        enhanced = cv2.resize(enhanced, (w * 2, h * 2), interpolation=cv2.INTER_CUBIC)
    return enhanced


def _paddle_text(img) -> str:
    from paddleocr import PaddleOCR  # heavyweight import; only when used
    if not hasattr(_paddle_text, "_ocr"):
        _paddle_text._ocr = PaddleOCR(
            lang="en", use_doc_orientation_classify=False,
            use_doc_unwarping=False, use_textline_orientation=False,
            enable_mkldnn=False,  # paddle 3.3 oneDNN PIR crash (see pipeline)
            text_det_limit_side_len=1600, text_det_limit_type="max",
            text_recognition_batch_size=1)
    ocr = _paddle_text._ocr
    h, w = img.shape[:2]
    if h * w <= _OCR_MAX_PIXELS:
        bands = [(0, h)]
    else:
        step = max(1, (_OCR_MAX_PIXELS // w) - _OCR_BAND_OVERLAP)
        bands, y = [], 0
        while y < h:
            bands.append((y, min(h, y + step + _OCR_BAND_OVERLAP)))
            y += step
    out = []
    for y0, y1 in bands:
        crop = img[y0:y1]
        if crop.ndim == 2:
            crop = cv2.cvtColor(crop, cv2.COLOR_GRAY2BGR)
        for r in ocr.predict(crop):
            out.extend(r.get("rec_texts", []))
    return "\n".join(out)


def ocr_page(path: str) -> str:
    """The deterministic OCR chain under test: periodic-watermark removal +
    denoise + upscale, then the primary engine with a fallback chain scored by
    what each read EXTRACTS (confetti noise is long but field-free)."""
    gray = cv2.cvtColor(cv2.imread(path), cv2.COLOR_BGR2GRAY)
    enhanced = _enhance(gray)

    if os.environ.get("EVAL_OCR", "paddle") != "tesseract":
        try:
            return _paddle_text(enhanced)
        except Exception as e:  # engine unavailable/OOM -> offline floor
            print(f"  [paddle unavailable: {type(e).__name__}; falling back to tesseract]",
                  file=sys.stderr)

    import pytesseract
    best, best_score = "", -1.0
    for psm in (3, 6, 11):
        text = (pytesseract.image_to_string(enhanced, config=f"--psm {psm}") or "").strip()
        s = _fields.extraction_score(text)
        if s > best_score:
            best, best_score = text, s
    return best


def _squash(s: str) -> str:
    return re.sub(r"[^a-z0-9]", "", s.lower())


def score_document(text: str, truth_fields: dict) -> dict:
    """Per-field hit/miss against ground truth. A field hits when its value
    (or the value from typed extraction) appears in the OCR text,
    punctuation/case-tolerant — the same grounding standard verify() uses.
    Typed extraction is the channel the platform actually consumes, so a
    validated extraction hit counts for every typed field family (money,
    dates, identifiers, codes), not just money."""
    ex = _fields.extract_fields(text)
    hay = _squash(text)
    ids = ex.get("identifiers", {})
    id_values = {str(v.get("value")) if isinstance(v, dict) else str(v)
                 for v in ids.values() if v}
    if "taxonomy_repaired" in ids:
        id_values.add(str(ids["taxonomy_repaired"]))
    dx_values = set(ex.get("diagnosis_codes", []))
    date_isos = {d.get("iso") for d in ex.get("dates", [])}
    code_values = {c for c in ex.get("cpt_codes", [])} | {c for c in ex.get("hcpcs_codes", [])}
    remark_values = set(ex.get("adjustment_codes", []))
    results: dict[str, bool] = {}
    for name, want in truth_fields.items():
        want_s = str(want)
        hit = _squash(want_s) in hay
        if not hit and name.endswith("_cents"):
            cents = int(round(float(want_s.replace("$", "").replace(",", "")) * 100))
            hit = any(a["cents"] == cents for a in ex.get("amounts", []))
        if not hit and ("date" in name or name == "service_date"):
            # compare ISO-normalized: truth "03/12/2026" -> 2026-03-12
            iso = _fields._norm_date_token(want_s)
            hit = bool(iso) and iso in date_isos
        if not hit and name.startswith(("cpt:", "hcpcs:")):
            hit = want_s in code_values
        if not hit and name in ("denial_code", "remark_code"):
            hit = want_s.upper() in remark_values
        if not hit and name == "diagnosis_code":
            hit = want_s.upper() in dx_values
        if not hit and name in ("provider_npi", "npi", "provider_tin", "tin",
                                "member_id", "claim_number", "taxonomy"):
            hit = _squash(want_s) in {_squash(v) for v in id_values}
        results[name] = hit
    return results


# --- optional VLM rescue stage (mirrors production routing) -----------------
# Production behaviour: pages whose OCR read is field-poor route to the local
# VLM (qwen2.5vl via Ollama/vLLM) with atomic per-field questions. The eval
# reproduces that path when an endpoint is configured; without one it reports
# OCR-stage scores only and says so plainly.

_VLM_QUESTIONS = {
    "payer_name": "exact name of the health plan / insurance payer?",
    "provider_name": "exact name of the rendering or billing provider/facility?",
    "member_name": "patient / plan member full name?",
    "member_id": "member ID / subscriber ID? Preserve exact characters.",
    "npi": "rendering provider NPI (10 digits)?",
    "provider_npi": "rendering provider NPI (10 digits)?",
    "tin": "federal tax ID / EIN (9 digits)?",
    "provider_tin": "federal tax ID / EIN (9 digits)?",
    "taxonomy": "provider taxonomy code (e.g. 207Q00000X)?",
    "claim_number": "claim / encounter number? Preserve exact characters.",
    "service_date": "date of service?",
    "determination_date": "determination / decision / process date?",
    "place_of_service": "place of service code?",
    "network_status": "in-network or out-of-network?",
    "diagnosis_code": "primary ICD-10 diagnosis code?",
    "denial_code": "remark / adjustment / denial reason codes (e.g. CO-45)?",
    "billed_cents": "total billed amount (USD)?",
    "allowed_cents": "allowed amount (USD)?",
    "plan_paid_cents": "amount the plan paid (USD)?",
    "patient_resp_cents": "patient responsibility amount (USD)?",
    "total_cents": "total charge amount (USD)?",
}


def _vlm_available(endpoint: str) -> bool:
    import urllib.request
    try:
        with urllib.request.urlopen(endpoint.rstrip("/") + "/models", timeout=3) as r:
            return r.status == 200
    except Exception:
        try:
            import requests
            r = requests.get(endpoint.rstrip("/").replace("/v1", "") + "/api/tags", timeout=3)
            return r.status_code == 200
        except Exception:
            return False


def vlm_rescue(path: str, truth_fields: dict, doc_type: str, endpoint: str,
               model: str) -> str:
    """Atomic-question VLM extraction over the enhanced page image; returns
    synthetic 'label: value' text to merge with the OCR read. Mirrors
    pipeline.stage_vlm_extract's question decomposition and the
    never-invent-anything grounding rule."""
    import base64, io
    import requests
    img = cv2.imread(path, 0)
    enh = _enhance(img)
    keys = [k for k in truth_fields if not k.startswith(("cpt:", "hcpcs:"))]
    questions = "\n".join(
        f'- "{k}": {_VLM_QUESTIONS.get(k, k.replace("_", " ") + "?")}' for k in keys)
    prompt = (
        "You are an IDR (No Surprises Act dispute) document analyst. This is a "
        f"'{doc_type}' document image. Answer EACH extraction question "
        "independently. Reply as one JSON object: {\"<field>\": {\"value\": ..., "
        "\"probability\": 0-1}, ...}. null value when the field is absent. Only "
        "extract values that literally appear in the document — never invent or "
        "infer identifiers or amounts. Preserve exact formatting of codes and "
        "identifiers. Amounts as numbers in USD, dates as they appear.\n"
        f"QUESTIONS:\n{questions}\n")
    thumb = Image.fromarray(enh)
    thumb.thumbnail((1400, 1400))
    buf = io.BytesIO()
    thumb.convert("L").save(buf, "JPEG", quality=92)
    b64 = base64.b64encode(buf.getvalue()).decode()
    r = requests.post(endpoint.rstrip("/") + "/chat/completions", json={
        "model": model, "max_tokens": 1600,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": prompt},
            {"type": "image_url", "image_url": {"url": f"data:image/jpeg;base64,{b64}"}}]}]},
        timeout=900)
    body = r.json()["choices"][0]["message"]["content"]
    m = re.search(r"\{.*\}", body, re.S)
    if not m:
        return ""
    try:
        parsed = json.loads(m.group(0))
    except json.JSONDecodeError:
        return ""
    lines = []
    for k, v in parsed.items():
        val = v.get("value") if isinstance(v, dict) else v
        if val not in (None, "", "null"):
            lines.append(f"{k.replace('_', ' ')}: {val}")
    return "\n".join(lines)


def main() -> int:
    fixture_dir = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.path.dirname(__file__), "testdata", "eval")
    with open(os.path.join(fixture_dir, "ground_truth.json")) as f:
        truth = json.load(f)

    vlm_endpoint = os.environ.get("VLM_ENDPOINT", "")
    vlm_model = os.environ.get("VLM_MODEL", "qwen2.5vl:7b")
    vlm_on = bool(vlm_endpoint) and _vlm_available(vlm_endpoint)
    if vlm_endpoint and not vlm_on:
        print(f"VLM_ENDPOINT={vlm_endpoint} unreachable — OCR-stage scoring only")

    total = hits = 0
    for fname, spec in truth.items():
        path = os.path.join(fixture_dir, fname)
        text = ocr_page(path)
        doc_type, score, _margin = ("", 0, 0)
        try:
            from pipeline import classify_text
            doc_type, score, _margin = classify_text(fname, text)
        except Exception:
            pass
        results = score_document(text, spec["fields"])
        routed = ""
        # Production routing: a field-poor OCR read goes to the VLM with
        # atomic per-field questions (pipeline.stage_vlm_extract).
        if vlm_on and sum(results.values()) < 0.6 * len(results):
            rescue = vlm_rescue(path, spec["fields"],
                                doc_type or spec.get("doc_type", "document"),
                                vlm_endpoint, vlm_model)
            if rescue:
                results = score_document(text + "\n" + rescue, spec["fields"])
                routed = "  [VLM-routed]"
        type_hit = doc_type == spec["doc_type"]
        got = sum(results.values())
        total += len(results) + 1
        hits += got + int(type_hit)
        misses = [k for k, ok in results.items() if not ok]
        print(f"{fname}: {got}/{len(results)} fields"
              f"{' +type' if type_hit else ' (type MISS: got ' + repr(doc_type) + ')'}"
              + routed
              + (f"  misses: {', '.join(misses)}" if misses else ""))

    acc = hits / total if total else 0.0
    print(f"\nOVERALL: {hits}/{total} = {acc:.1%}")
    minimum = float(os.environ.get("ACCURACY_MIN", "0"))
    if acc < minimum:
        print(f"BELOW GATE (ACCURACY_MIN={minimum:.0%})")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
