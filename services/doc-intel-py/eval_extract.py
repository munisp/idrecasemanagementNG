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
            text_det_limit_side_len=1600, text_recognition_batch_size=1)
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
    punctuation/case-tolerant — the same grounding standard verify() uses."""
    ex = _fields.extract_fields(text)
    hay = _squash(text)
    results: dict[str, bool] = {}
    for name, want in truth_fields.items():
        want_s = str(want)
        hit = _squash(want_s) in hay
        # Money fields: also accept the typed-extraction match (label-tagged
        # cents equal), which is what actually feeds eligibility evidence.
        if not hit and name.endswith("_cents"):
            cents = int(round(float(want_s.replace("$", "").replace(",", "")) * 100))
            hit = any(a["cents"] == cents for a in ex.get("amounts", []))
        results[name] = hit
    return results


def main() -> int:
    fixture_dir = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
        os.path.dirname(__file__), "testdata", "eval")
    with open(os.path.join(fixture_dir, "ground_truth.json")) as f:
        truth = json.load(f)

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
        type_hit = doc_type == spec["doc_type"]
        got = sum(results.values())
        total += len(results) + 1
        hits += got + int(type_hit)
        misses = [k for k, ok in results.items() if not ok]
        print(f"{fname}: {got}/{len(results)} fields"
              f"{' +type' if type_hit else ' (type MISS: got ' + repr(doc_type) + ')'}"
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
