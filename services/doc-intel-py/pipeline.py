"""Composable document-analysis stages (Docling-first, PaddleOCR fallback).

Each stage is a function stage_<name>(ctx) -> ctx that reads/writes keys on a
shared context dict. pipeline.yaml decides which stages run, in what order, and
under what condition (`when: <ctx-flag>`): Docling is the primary parser,
PaddleOCR is conditionally docked for scanned pages, PP-StructureV3 handles
seal/stamp detection, and the VLM stage does semantic extraction.

Accuracy architecture (v2):
  classify (weighted, margin-aware) -> docling -> ocr? -> layout
    -> classify (2nd pass) -> vlm_extract (per-doc-type schema, chunked,
       multi-page, self-repairing JSON, transport retries)
    -> normalize (deterministic canonicalization of amounts/dates/codes)
    -> verify (grounding check: every extracted value must literally appear
       in the document text; per-field confidence)
    -> validate (cross-checks vs case record + review routing)
    -> index

Performance architecture (v3):
  * Heavy models (Docling converter, PaddleOCR, PP-StructureV3) are process-
    level SINGLETONS keyed by their config — construction is model-loading
    from disk, so per-document construction was multiplying every document's
    latency by seconds and churning worker memory (a real OOM vector on a
    multi-tenant queue).
  * VLM chunk extraction runs CONCURRENTLY (config: parallel_chunks) — the
    calls are network-bound, and a 40-page itemized bill was paying N serial
    round-trips. Merge order is preserved; results are deterministic.
  * Scan-quality assessment measures on downscaled pages with a Gaussian
    pre-filter — fastNlMeansDenoising is a beautiful denoiser and a terrible
    thing to run on every page of every document on CPU.
  * Degraded-scan recovery re-renders and enhances ONLY the poor pages.
  * verify() squashes the document haystack ONCE, not per extracted value;
    VLM page images ship as JPEG, not PNG (5-10x smaller, faster encode).
"""

from __future__ import annotations

import io
import json
import os
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from typing import Any

import httpx
import yaml
from PIL import Image

# ---------------------------------------------------------------------------
# Model singletons — construction IS model loading; never per-document
# ---------------------------------------------------------------------------

_MODEL_CACHE: dict[tuple, Any] = {}
_MODEL_LOCK = threading.Lock()


def _model(key: tuple, factory) -> Any:
    """Process-level singleton for heavy model objects. Double-checked
    locking: workers are typically single-threaded per replica, but the VLM
    stage already parallelizes and stage code must stay safe under threads."""
    if key not in _MODEL_CACHE:
        with _MODEL_LOCK:
            if key not in _MODEL_CACHE:
                _MODEL_CACHE[key] = factory()
    return _MODEL_CACHE[key]

# ---------------------------------------------------------------------------
# Stage implementations
# ---------------------------------------------------------------------------

# Weighted keyword signals per doc type. Strong signals (weight 3) are phrases
# that essentially only appear in that document type; weak signals (weight 1)
# are common billing vocabulary that needs corroboration. Classification runs
# twice: pass 1 (filename only, before parsing) and pass 2 (full text after
# Docling/OCR — a misleading filename can't win).
TYPE_SIGNALS: dict[str, dict[str, list[str]]] = {
    "eob": {
        "strong": ["explanation of benefits", "remittance advice", "patient responsibility",
                   "allowed amount", "adjustment amount", "claim adjustment reason"],
        "weak": ["claim number", "denial code", "coinsurance", "copay", "deductible",
                 "eob", "benefits", "covered", "not covered"],
    },
    "determination_letter": {
        "strong": ["independent dispute resolution", "certified idre", "prevailing party",
                   "offer selected", "45 cfr", "dispute resolution entity",
                   "notice of idr determination", "determination of payment"],
        "weak": ["determination", "out-of-network rate", "dispute", "arbitration",
                 "idre", "batched", "attest"],
    },
    "idr_claim": {
        "strong": ["qualifying payment amount", "ub-04", "cms-1500", "hcpcs",
                   "place of service", "taxonomy", "itemized bill"],
        "weak": ["cpt", "billed amount", "qpa", "service date", "npi", "diagnosis code",
                 "procedure code", "claim", "provider", "payer", "member id", "units",
                 "revenue code", "drg"],
    },
}
WEIGHT = {"strong": 3, "weak": 1}

# Minimum score to accept a classification at all, and minimum lead over the
# runner-up to accept it without an ambiguity flag.
CLASSIFY_MIN_SCORE = 2
CLASSIFY_MIN_MARGIN = 2


def classify_text(name: str, text: str) -> tuple[str, int, int]:
    """(best_type, best_score, margin). ('unrelated', 0, 0) when no signal."""
    body = (text or "").lower()[:20000]
    name_l = (name or "").lower()
    scores: dict[str, int] = {}
    for dtype, tiers in TYPE_SIGNALS.items():
        s = 0
        for tier, signals in tiers.items():
            w = WEIGHT[tier]
            for sig in signals:
                if sig in body:
                    s += w
                # Filename hits count double — names are deliberate, body noisy.
                if sig in name_l:
                    s += 2 * w
        scores[dtype] = s
    ranked = sorted(scores.items(), key=lambda kv: kv[1], reverse=True)
    best, best_score = ranked[0]
    margin = best_score - ranked[1][1]
    if best_score < CLASSIFY_MIN_SCORE:
        return "unrelated", 0, 0
    return best, best_score, margin


def stage_classify(ctx: dict, cfg: dict) -> dict:
    """Doc-type guess: filename + (when available) extracted text. Documents
    with no IDR signal are typed 'unrelated'; a winning type with a thin
    margin over the runner-up is kept but flagged ambiguous — validate()
    routes ambiguous docs for a human glance instead of trusting the coin
    flip."""
    name = ctx.get("filename") or ""
    dtype, score, margin = classify_text(name, ctx.get("text", ""))
    ctx["doc_type"] = dtype
    ctx["classify_score"] = score
    ctx["classify_ambiguous"] = bool(dtype != "unrelated" and margin < CLASSIFY_MIN_MARGIN)
    return ctx


def stage_docling(ctx: dict, cfg: dict) -> dict:
    """PRIMARY parser — IBM Docling: PDF/DOCX/PPTX/HTML/images into a structured
    document (reading order, layout regions, tables, figures, formulas)."""
    from docling.datamodel.base_models import DocumentStream

    def build():
        from docling.datamodel.pipeline_options import PdfPipelineOptions
        from docling.document_converter import DocumentConverter, PdfFormatOption
        from docling.datamodel.base_models import InputFormat
        opts = PdfPipelineOptions(
            do_ocr=cfg.get("do_ocr", True),
            do_table_structure=cfg.get("do_table_structure", True),
        )
        return DocumentConverter(
            format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=opts)}
        )

    converter = _model(("docling", cfg.get("do_ocr", True),
                        cfg.get("do_table_structure", True)), build)
    stream = DocumentStream(name=ctx.get("filename", "doc.pdf"),
                            stream=io.BytesIO(ctx["raw_bytes"]))
    doc = converter.convert(stream).document

    ctx["text"] = doc.export_to_markdown()            # structure-aware reading order
    ctx["markdown"] = ctx["text"]
    ctx["tables"] = [
        t.export_to_dataframe().to_dict(orient="records")
        for t in getattr(doc, "tables", [])
    ]
    ctx["layout_regions"] = [
        {"label": getattr(item, "label", "text"), "content": getattr(item, "text", "")[:2000]}
        for item in getattr(doc, "texts", [])
    ]
    # Low text coverage => scanned document => conditionally dock PaddleOCR.
    n_pages = max(1, len(ctx.get("pages", [])) or 1)
    ctx["low_text_coverage"] = len(ctx["text"].strip()) < 40 * n_pages
    return ctx


# Scan-quality thresholds (empirical; tune against your document mix).
# NOTE: blur is now measured on 1400px-downscaled, Gaussian-smoothed pages
# (see assess_page_quality) — a different variance scale than the original
# full-res NLM measurement. 80.0 remains a sane focus floor at this scale,
# but recalibrate against a labeled sample of your real scan mix.
_BLUR_MIN = 80.0        # Laplacian variance (denoised) below this => out of focus
_SEPARATION_MIN = 40.0  # bg median - fg p5 below this => faint/washed-out print
_BRIGHTNESS_LO = 60.0   # mean below => underexposed (overexposure is caught
                        # by separation: washed-out pages have no dark ink)


def assess_page_quality(img: Image.Image) -> dict:
    """Objective scan-quality metrics via OpenCV (bundled with PaddleOCR):
    - blur: Laplacian variance, measured after light denoising (raw variance
      is inflated by sensor noise, which masquerades as sharpness)
    - separation: background median minus foreground 5th percentile — the
      ink-to-paper gap. Whole-page stddev is useless here: sparse crisp text
      scores low on it, while a faint scan's defining trait is exactly that
      its darkest pixels never get dark.
    - exposure: mean brightness out of range.
    Returns metrics + a poor flag. A 'valid but barely legible' document is
    detected HERE — before OCR — so the pipeline can enhance and reviewers
    get an accurate diagnosis instead of a generic 'no content' finding."""
    import cv2

    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    # Downscale first: blur/separation/exposure are stable at assessment
    # scale, and a 300dpi page is ~8MP of wasted arithmetic otherwise.
    h, w = gray.shape
    if max(h, w) > 1400:
        s = 1400 / max(h, w)
        gray = cv2.resize(gray, (int(w * s), int(h * s)), interpolation=cv2.INTER_AREA)
    # Gaussian pre-filter in place of fastNlMeansDenoising: NLM is the right
    # denoiser for RESTORATION, but for a variance measurement it only needs
    # sensor noise suppressed — a 5x5 Gaussian does that in microseconds
    # where NLM costs seconds per page on CPU.
    smooth = cv2.GaussianBlur(gray, (5, 5), 0)
    blur = float(cv2.Laplacian(smooth, cv2.CV_64F).var())
    # Separation = how much darker the ink is than the paper, measured over
    # actual ink pixels (anything below near-white). Percentile-of-page
    # approaches fail on sparse pages; whole-page stddev fails everywhere.
    ink = gray[gray < 240]
    separation = float(240 - ink.mean()) if ink.size >= 100 else 0.0
    brightness = float(gray.mean())
    poor = (blur < _BLUR_MIN or separation < _SEPARATION_MIN
            or brightness < _BRIGHTNESS_LO)
    return {"blur": round(blur, 1), "separation": round(separation, 1),
            "brightness": round(brightness, 1), "poor": poor}


def preprocess_for_ocr(img: Image.Image) -> Image.Image:
    """Enhancement pass for degraded scans before OCR: grayscale, CLAHE local
    contrast (recovers faint print), fast denoise, and 2x upscale when the
    page is small (sub-150dpi effective resolution starves the detector)."""
    import cv2

    gray = cv2.cvtColor(np_from_pil(img), cv2.COLOR_RGB2GRAY)
    clahe = cv2.createCLAHE(clipLimit=3.0, tileGridSize=(8, 8))
    enhanced = clahe.apply(gray)
    # Median 3x3 in place of NLM(h=10): on salt-and-pepper scan noise the
    # OCR-accuracy difference is negligible and the speed difference is
    # orders of magnitude (NLM on a 300dpi page = multi-second, per page).
    enhanced = cv2.medianBlur(enhanced, 3)
    h, w = enhanced.shape
    if max(h, w) < 2000:
        enhanced = cv2.resize(enhanced, (w * 2, h * 2), interpolation=cv2.INTER_CUBIC)
    return Image.fromarray(enhanced)


def stage_ocr(ctx: dict, cfg: dict) -> dict:
    """PaddleOCR full-text extraction, with scan-quality triage:
    1. Assess every page (blur/contrast/exposure).
    2. Poor-quality PDF pages are RE-RENDERED at 300dpi from the original
       bytes (the initial 200dpi rasterization starves OCR of detail), then
       enhanced (contrast/denoise/upscale).
    3. OCR runs over the best available image of each page.
    ctx['scan_quality_poor'] tells validate() to demand human verification."""
    from paddleocr import PaddleOCR

    pages = ensure_pages(ctx, dpi=cfg.get("render_dpi", 200))
    qualities = [assess_page_quality(p) for p in pages[:10]]  # sample bound
    poor_pages = [i for i, q in enumerate(qualities) if q["poor"]]
    ctx["scan_quality"] = qualities
    if poor_pages and len(poor_pages) >= max(1, len(qualities) // 2):
        ctx["scan_quality_poor"] = True
        # Re-render ONLY the poor PDF pages at higher DPI (the bytes hold
        # more detail than the 200dpi rasterization), and enhance ONLY
        # those pages — re-rendering and CLAHE/denoise-upscaling a full
        # 40-page document when 3 pages are bad was the OCR stage's
        # single largest time sink. Images are used as-is.
        if sniff_doc_kind(ctx.get("raw_bytes", b""), ctx.get("content_type", "")) == "pdf":
            hires, _ = pdf_to_pages(ctx["raw_bytes"], dpi=cfg.get("enhance_dpi", 300))
            if hires:
                for i in poor_pages:
                    if i < len(hires):
                        pages[i] = hires[i]
                ctx["pages"] = pages
        for i in poor_pages:
            pages[i] = preprocess_for_ocr(pages[i])
        ctx["ocr_enhanced"] = True

    # PaddleOCR construction loads the detector+recognizer from disk — a
    # process singleton, not a per-document cost.
    ocr = _model(
        ("paddleocr", cfg.get("lang", "en"),
         cfg.get("use_doc_orientation_classify", True),
         cfg.get("use_doc_unwarping", True),
         cfg.get("use_textline_orientation", True)),
        lambda: PaddleOCR(
            lang=cfg.get("lang", "en"),
            use_doc_orientation_classify=cfg.get("use_doc_orientation_classify", True),
            use_doc_unwarping=cfg.get("use_doc_unwarping", True),
            use_textline_orientation=cfg.get("use_textline_orientation", True),
        ),
    )
    # Batched inference: PaddleOCR 3.x predict() accepts a LIST of images
    # and pipelines detection/recognition across them internally — per-page
    # calls pay Python-side and scheduler overhead on every page of every
    # scanned document. Small batches bound the memory spike (each page is
    # a full bitmap in worker RAM). Falls back to per-page if a Paddle
    # version rejects list input — OCR must never fail the document.
    texts: list[str] = []
    arrays = [np_from_pil(p) for p in pages]
    batch_n = max(1, int(cfg.get("batch_pages", 4)))

    def collect(results) -> None:
        for res in results:
            texts.extend(res.get("rec_texts", []))

    try:
        for i in range(0, len(arrays), batch_n):
            collect(ocr.predict(arrays[i:i + batch_n]))
    except Exception:
        texts.clear()
        for arr in arrays:
            collect(ocr.predict(arr))
    ctx["text"] = "\n".join(texts)
    # OCR recovered text for a scanned doc: markdown has no structure, but
    # downstream stages read markdown first, so mirror it.
    ctx["markdown"] = ctx["text"]
    return ctx


def stage_layout(ctx: dict, cfg: dict) -> dict:
    """PP-StructureV3 layout analysis: regions, reading order, seals/stamps."""
    from paddleocr import PPStructureV3

    engine = _model(
        ("ppstructure", cfg.get("use_table_recognition", True),
         cfg.get("use_seal_recognition", True)),
        lambda: PPStructureV3(
            use_table_recognition=cfg.get("use_table_recognition", True),
            use_seal_recognition=cfg.get("use_seal_recognition", True),
        # Known PaddlePaddle 3.3.x regression: the oneDNN CPU backend's PIR
        # attribute converter has no case for ArrayAttribute<DoubleAttribute>,
        # which several of this pipeline's models hit (upstream issue
        # PaddlePaddle/Paddle#79749/#77340). Confirmed live. No fix upstream
        # yet -- enable_mkldnn=False is the documented workaround.
            enable_mkldnn=False,
        ),
    )
    regions: list[dict] = []
    for i, page in enumerate(ensure_pages(ctx)):
        for res in engine.predict(np_from_pil(page)):
            for blk in res.get("parsing_res_list", []):
                regions.append({
                    "page": i,
                    "label": blk.get("block_label"),
                    "content": blk.get("block_content", "")[:2000],
                })
    ctx["layout_regions"] = regions
    ctx["seal_detected"] = any(r["label"] == "seal" for r in regions)
    return ctx


# ---------------------------------------------------------------------------
# VLM extraction
# ---------------------------------------------------------------------------

def _vlm_call(client: Any, model: str, content: list[dict], max_tokens: int) -> dict:
    """One VLM round-trip with transport retries and JSON self-repair.

    Retries up to 3 times on transport/5xx errors with exponential backoff.
    If the model returns unparseable JSON, sends one repair follow-up asking
    for the same answer as bare JSON before giving up. Raises only when all
    attempts fail."""
    last_exc: Exception | None = None
    messages = [{"role": "user", "content": content}]
    for attempt in range(3):
        try:
            resp = client.chat.completions.create(
                model=model, messages=messages,
                max_tokens=max_tokens, temperature=0,
            )
            raw = resp.choices[0].message.content or ""
            try:
                return _parse_json_object(raw)
            except ValueError:
                # Self-repair: one follow-up demanding strict JSON, then fail.
                messages = messages + [
                    {"role": "assistant", "content": raw},
                    {"role": "user", "content": (
                        "Your previous answer was not valid JSON. Reply with the "
                        "SAME answer as a single bare JSON object only — no prose, "
                        "no markdown fences, no explanation.")},
                ]
                resp2 = client.chat.completions.create(
                    model=model, messages=messages,
                    max_tokens=max_tokens, temperature=0,
                )
                return _parse_json_object(resp2.choices[0].message.content or "")
        except (httpx.HTTPError, ConnectionError, TimeoutError) as exc:
            last_exc = exc
            time.sleep(0.5 * (2 ** attempt))
    raise RuntimeError(f"VLM call failed after retries: {last_exc}")


def _parse_json_object(raw: str) -> dict:
    """Strict parse first; fall back to the largest {...} span; else ValueError."""
    raw = raw.strip()
    if raw.startswith("```"):
        raw = re.sub(r"^```(json)?\s*|\s*```$", "", raw, flags=re.S).strip()
    try:
        obj = json.loads(raw)
        if isinstance(obj, dict):
            return obj
    except json.JSONDecodeError:
        pass
    m = re.search(r"\{.*\}", raw, re.S)
    if m:
        try:
            obj = json.loads(m.group(0))
            if isinstance(obj, dict):
                return obj
        except json.JSONDecodeError:
            pass
    raise ValueError("no parseable JSON object in VLM response")


def _chunk_markdown(md: str, chunk_chars: int) -> list[str]:
    """Split long markdown on paragraph boundaries so no field-bearing line is
    cut mid-sentence; keeps every chunk under chunk_chars."""
    if len(md) <= chunk_chars:
        return [md]
    chunks, buf = [], ""
    for para in re.split(r"\n\s*\n", md):
        if buf and len(buf) + len(para) + 2 > chunk_chars:
            chunks.append(buf)
            buf = para
        else:
            buf = (buf + "\n\n" + para) if buf else para
    if buf:
        chunks.append(buf)
    return chunks


def _merge_extractions(parts: list[dict], fields: list[str]) -> dict:
    """Merge per-chunk extractions: first non-null value wins; lists are
    unioned (order-preserving, de-duplicated)."""
    merged: dict[str, Any] = {f: None for f in fields}
    for f in fields:
        for part in parts:
            v = part.get(f)
            if v in (None, "", [], {}):
                continue
            if isinstance(v, list):
                cur = merged[f] if isinstance(merged[f], list) else []
                merged[f] = cur + [x for x in v if x not in cur]
            elif merged[f] in (None, "", [], {}):
                merged[f] = v
    return merged


def stage_vlm_extract(ctx: dict, cfg: dict, schemas: dict) -> dict:
    """VLM semantic extraction via an OpenAI-compatible endpoint
    (PaddleOCR-VL or Qwen2.5-VL served by vLLM; swap with one env var).

    - Schema follows the classified doc_type (a determination letter is asked
      for prevailing_party, not CPT codes); unknown/unrelated types fall back
      to idr_claim so mis-uploads still get probed for any billing content.
    - Documents longer than chunk_chars are extracted per chunk and merged —
      a 40-page itemized bill no longer loses everything past character 10k.
    - Up to max_pages page images accompany the text for layout cues.
    """
    # Client is a process singleton per endpoint: TCP keep-alive to the
    # local Ollama survives across documents instead of re-handshaking per
    # doc, and the OpenAI client is thread-safe for the parallel chunks.
    def build_client():
        from openai import OpenAI
        return OpenAI(base_url=cfg["endpoint"],
                      api_key=os.environ.get("VLM_API_KEY", "none"),
                      timeout=httpx.Timeout(180.0, connect=10.0))

    client = _model(("vlm_client", cfg["endpoint"]), build_client)
    doc_type = ctx.get("doc_type") or "idr_claim"
    schema_name = doc_type if doc_type in schemas else cfg.get("schema", "idr_claim")
    ctx["schema_used"] = schema_name
    fields = schemas[schema_name]["fields"]

    md = ctx.get("markdown", ctx.get("text", ""))
    # Docling's own markdown export already renders detected tables as GFM
    # markdown tables inline in `md` -- this JSON blob is largely the SAME
    # table data a second time, structured differently. Sized small since
    # MAX_PROMPT_CHARS below hard-caps the whole prompt at 2000 chars
    # anyway -- this just avoids that cap being spent entirely on a
    # redundant copy of content `md` already has.
    tables_hint = json.dumps(ctx.get("tables", [])[:1])[:300]
    # Page images only when the configured model actually consumes them —
    # with the text-only local Ollama default, rasterizing pages for the
    # VLM is pure waste (ensure_pages renders lazily on first real need).
    pages = ensure_pages(ctx) if cfg.get("vision") else ctx.get("pages", [])
    max_pages = cfg.get("max_pages", 3)

    instructions = (
        "You are an IDR (No Surprises Act dispute) document analyst. "
        "FIRST decide whether this document is related to medical billing, health "
        "insurance claims, explanation-of-benefits, or IDR arbitration at all, and "
        'set "_in_domain" true or false. A resume, menu, tax form, legal contract, '
        "photograph with no document content, or any other unrelated material is "
        "false. When false, set every other field null and stop. "
        f"This document is believed to be of type '{doc_type}'. "
        f"Extract these fields as strict JSON (null when absent): {', '.join(fields)}.\n"
        "Only extract values that literally appear in the document — never invent "
        "or infer identifiers or amounts. Preserve exact formatting of codes and "
        "identifiers (CPT/HCPCS/NDC, claim numbers, NPIs). Amounts as numbers in "
        "USD, dates as they appear.\n"
    )

    chunk_chars = cfg.get("chunk_chars", 12000)
    chunks = _chunk_markdown(md, chunk_chars)

    def run_chunk(i: int, chunk: str) -> dict:
        prompt = instructions
        if len(chunks) > 1:
            prompt += f"This is section {i + 1} of {len(chunks)} of one document.\n"
        prompt += "Document content (markdown with layout) follows, then extracted tables.\n\n" + chunk
        if i == 0:
            prompt += "\n\nTABLES:\n" + tables_hint
        # Hard backstop. Every chars-per-token ratio assumed here so far has
        # been wrong, twice: billing content (CPT/HCPCS codes, NPIs, dollar
        # amounts, dates, MICR strings) is mostly digits and symbols, and
        # that tokenizes far denser than English prose, by an amount that
        # keeps moving the goalposts (9000 chars -> 4581 tokens, then 6000
        # chars -> 4532 tokens -- the real ratio got WORSE, not better, when
        # the cap shrank, meaning density isn't even constant across
        # documents). Stop estimating a ratio and just cap low enough that
        # no plausible density can overflow: 2000 chars would need under
        # 0.49 chars/token to still exceed 4096 -- not a real tokenizer
        # outcome for any text, JSON, or digit-heavy content.
        MAX_PROMPT_CHARS = 2000
        if len(prompt) > MAX_PROMPT_CHARS:
            prompt = prompt[:MAX_PROMPT_CHARS]
        content: list[dict] = [{"type": "text", "text": prompt}]
        # Multimodal: page images give layout cues text alone loses -- but
        # only when the configured model actually supports it. The deployed
        # model/vision flag found live (VLM_MODEL=qwen2.5vl:7b, VLM_VISION=
        # true) don't match this file's own long-standing comments assuming
        # a text-only model with vision off -- someone upgraded the live
        # config without updating here. With vision genuinely on, every
        # call attached a FULL page bitmap straight from pdf_to_pages'
        # 200-300dpi render (1700x2200+ px for a letter page) -- the real
        # source of every "exceeds context size" failure this prompt-char
        # cap could never fix, since image tokens aren't text and this cap
        # never touched them. Confirmed live against this exact endpoint/
        # model: a 2000-char text prompt alone costs ~1030 prompt tokens,
        # but the SAME request with a page image attached costs ~2095 --
        # ~1065 tokens of pure image cost -- and that number held constant
        # (not proportional) across every size tried from 350px up to
        # 612px on the long edge, i.e. a resize within that range is free:
        # it doesn't trade away any of the layout-cue fidelity the image
        # was for. Thumbnail to a small fixed box before encoding instead
        # of sending the raw full-DPI render.
        if cfg.get("vision"):
            max_px = cfg.get("vlm_image_max_px", 650)
            for page in pages[i * max_pages:(i + 1) * max_pages] or pages[:max_pages]:
                thumb = page.copy()
                thumb.thumbnail((max_px, max_px))
                content.append({"type": "image_url",
                                # upstream context fix (thumbnail) + JPEG encoding
                                "image_url": {"url": f"data:image/jpeg;base64,{pil_to_b64(thumb)}"}})
                break  # one page image per chunk keeps token cost bounded
        return _vlm_call(client, cfg["model"], content, cfg.get("max_tokens", 2048))

    # Chunk concurrency: the endpoint is the LOCAL in-cluster Ollama, NOT
    # OpenAI -- a shared platform instance with a known OOM history (see
    # pipeline.yaml). Ollama serializes past its own OLLAMA_NUM_PARALLEL
    # (default 1), so more workers than that just queue server-side while
    # multiplying KV-cache pressure. Default 2 overlaps request/JSON overhead
    # with inference; raise VLM_PARALLEL_CHUNKS only in lockstep with
    # OLLAMA_NUM_PARALLEL on the Ollama deployment.
    parallel = int(cfg.get("parallel_chunks",
                           os.environ.get("VLM_PARALLEL_CHUNKS", "2")))
    if len(chunks) > 1 and parallel > 1:
        with ThreadPoolExecutor(max_workers=min(parallel, len(chunks))) as pool:
            raw_parts = list(pool.map(lambda ic: run_chunk(*ic), enumerate(chunks)))
    else:
        raw_parts = [run_chunk(i, c) for i, c in enumerate(chunks)]

    parts: list[dict] = []
    in_domain_votes: list[bool] = []
    for parsed in raw_parts:
        in_domain_votes.append(bool(parsed.pop("_in_domain", True)))
        parts.append(parsed)

    ctx["in_domain"] = any(in_domain_votes) if in_domain_votes else True
    # Strict schema: only declared fields survive, all present (null-filled) —
    # the VLM can't smuggle invented keys into the record.
    ctx["extracted"] = _merge_extractions(parts, fields)
    return ctx


# ---------------------------------------------------------------------------
# Normalization + verification
# ---------------------------------------------------------------------------

_RE_AMOUNT = re.compile(r"^\$?\s*(-?[\d,]+(?:\.\d{1,2})?)$")
_RE_DATE = re.compile(
    r"^(\d{4})-(\d{2})-(\d{2})$|"                     # ISO
    r"^(\d{1,2})[/-](\d{1,2})[/-](\d{2,4})$|"          # US numeric
    r"^([A-Za-z]+)\s+(\d{1,2}),?\s+(\d{4})$"           # January 5, 2026
)
_MONTHS = {m.lower(): i + 1 for i, m in enumerate(
    ["January", "February", "March", "April", "May", "June", "July",
     "August", "September", "October", "November", "December"])}
_RE_CODE = re.compile(r"^[A-Z]?\d{4}[A-Z0-9]?$")       # CPT/HCPCS/DRG-ish
_RE_NPI = re.compile(r"^\d{10}$")


def _norm_amount(v: Any) -> float | None:
    if isinstance(v, (int, float)):
        return round(float(v), 2)
    if isinstance(v, str):
        m = _RE_AMOUNT.match(v.strip())
        if m:
            return round(float(m.group(1).replace(",", "")), 2)
    return None


def _norm_date(v: Any) -> str | None:
    """Canonicalize a date-ish value to ISO YYYY-MM-DD; None when unparseable."""
    if not isinstance(v, str):
        return None
    s = v.strip()
    m = _RE_DATE.match(s)
    if not m:
        return None
    if m.group(1):
        mm, dd = int(m.group(2)), int(m.group(3))
        if 1 <= mm <= 12 and 1 <= dd <= 31:
            return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
        return None
    if m.group(4):
        mm, dd, yy = int(m.group(4)), int(m.group(5)), m.group(6)
        yyyy = int(yy) if len(yy) == 4 else (2000 + int(yy) if int(yy) < 50 else 1900 + int(yy))
        if 1 <= mm <= 12 and 1 <= dd <= 31:
            return f"{yyyy:04d}-{mm:02d}-{dd:02d}"
        return None
    mon = _MONTHS.get(m.group(7).lower())
    if mon:
        return f"{int(m.group(9)):04d}-{mon:02d}-{int(m.group(8)):02d}"
    return None


def stage_normalize(ctx: dict, cfg: dict) -> dict:
    """Deterministic canonicalization of extracted values: amounts to floats,
    dates to ISO, identifiers de-formatted and shape-checked. Normalized
    copies live under ctx['normalized']; originals are never overwritten.
    Values that fail their shape check are recorded as normalize_warnings so
    verify()/validate() can weigh them."""
    ex = ctx.get("extracted", {})
    norm: dict[str, Any] = {}
    warnings: list[dict] = []
    for field, value in ex.items():
        if value in (None, "", [], {}):
            continue
        if field.endswith("_usd") or "amount" in field or field.startswith(("qpa", "billed", "allowed", "awarded", "patient_responsibility")):
            n = _norm_amount(value)
            if n is None:
                warnings.append({"field": field, "issue": f"amount not parseable: {value!r}"})
            else:
                norm[field] = n
        elif "date" in field:
            n = _norm_date(value)
            if n is None:
                warnings.append({"field": field, "issue": f"date not parseable: {value!r}"})
            else:
                norm[field] = n
        elif field == "provider_npi":
            digits = re.sub(r"\D", "", str(value))
            if _RE_NPI.match(digits):
                norm[field] = digits
            else:
                warnings.append({"field": field, "issue": "NPI is not 10 digits"})
        elif field in ("cpt_hcpcs_codes", "denial_codes") and isinstance(value, list):
            cleaned = [str(c).strip().upper() for c in value]
            bad = [c for c in cleaned if not _RE_CODE.match(c)]
            norm[field] = cleaned
            if bad:
                warnings.append({"field": field, "issue": f"implausible code(s): {', '.join(bad[:5])}"})
    ctx["normalized"] = norm
    ctx["normalize_warnings"] = warnings
    return ctx


def _flatten_values(v: Any) -> list[str]:
    """Scalar string forms of an extracted value (lists flattened one level)."""
    if isinstance(v, list):
        out: list[str] = []
        for x in v:
            out.extend(_flatten_values(x))
        return out
    if isinstance(v, (int, float)):
        return [str(v), f"{float(v):,.2f}", f"${float(v):,.2f}"]
    return [str(v)]


def _grounded(value_str: str, hay: str, hay_squashed: str) -> bool:
    """A value is grounded when it (or a whitespace/punctuation-tolerant form)
    literally appears in the document text. hay_squashed is precomputed once
    per document by the caller — squashing a 100k-char haystack per extracted
    value was O(fields x doc size) of pure waste."""
    needle = value_str.strip().lower()
    if not needle:
        return True
    if needle in hay:
        return True
    # Tolerate OCR/typography variance: collapse non-alphanumerics on both sides.
    squash_n = re.sub(r"[^a-z0-9]", "", needle)
    if len(squash_n) < 3:
        return needle in hay
    return squash_n in hay_squashed


def stage_verify(ctx: dict, cfg: dict) -> dict:
    """Grounding check: every extracted scalar must literally appear in the
    document text. Per-field confidence is recorded (high = grounded,
    low = not found in text). Ungrounded values are the hallucination
    signature — they become findings in validate() and are excluded from the
    trusted normalized set."""
    hay = (ctx.get("markdown") or ctx.get("text") or "").lower()
    hay_squashed = re.sub(r"[^a-z0-9]", "", hay)
    confidence: dict[str, str] = {}
    ungrounded: list[str] = []
    for field, value in ctx.get("extracted", {}).items():
        if value in (None, "", [], {}):
            continue
        atoms = [a for a in _flatten_values(value) if a.strip()]
        ok = all(_grounded(a, hay, hay_squashed) for a in atoms) if atoms else False
        confidence[field] = "high" if ok else "low"
        if not ok:
            ungrounded.append(field)
    ctx["field_confidence"] = confidence
    ctx["ungrounded_fields"] = ungrounded
    # Trusted normalized view: drop normalized copies of ungrounded fields.
    if ungrounded and ctx.get("normalized"):
        ctx["normalized"] = {k: v for k, v in ctx["normalized"].items() if k not in ungrounded}
    return ctx


# ---------------------------------------------------------------------------
# Validation + indexing
# ---------------------------------------------------------------------------

def stage_validate(ctx: dict, cfg: dict, case: dict | None) -> dict:
    """Cross-check extracted fields against the case record; mismatches flag."""
    # Accumulate onto whatever's already here -- a stage that failed earlier
    # and was skipped (run_pipeline's per-stage try/except) records itself as
    # a finding too; overwriting ctx["findings"] fresh here would silently
    # erase that, undoing the whole point of recording it.
    findings: list[dict] = list(ctx.get("findings", []))
    ex = ctx.get("extracted", {})
    norm = ctx.get("normalized", {})
    if case:
        qpa = norm.get("qpa_usd") or (_norm_amount(ex.get("qpa_usd")) if ex.get("qpa_usd") else None)
        if qpa and case.get("qpa_cents"):
            if abs(qpa * 100 - case["qpa_cents"]) > 100:
                findings.append({"field": "qpa_usd", "issue": "QPA mismatch vs case record"})
        if ex.get("claim_number") and case.get("case_number"):
            # claim vs CMS case number are different identifiers; record both
            ctx["extracted"]["cms_case_number"] = case["case_number"]
    # Out-of-domain guard: VLM verdict, zero-signal classification, or an
    # all-null extraction each independently mean a human should look before
    # this document is treated as case evidence.
    ex_fields = {k: v for k, v in ex.items() if v not in (None, "", [], {})}
    if ctx.get("in_domain") is False:
        findings.append({"field": None, "issue": "Document is not related to medical billing or IDR — possible mis-upload; routed for manual review"})
        ctx["doc_type"] = "unrelated"
    elif ctx.get("classify_score", 1) == 0 and not ex_fields and not ctx.get("scan_quality_poor"):
        findings.append({"field": None, "issue": "No IDR-relevant content detected — possible mis-upload; routed for manual review"})
    # Degraded scan: extraction ran on enhanced images but confidence in the
    # underlying text is inherently limited — say so plainly. A barely legible
    # VALID document must not be misdiagnosed as a mis-upload.
    if ctx.get("scan_quality_poor"):
        findings.append({"field": None, "issue": "Poor scan quality (blur/contrast/exposure) — extraction uncertain despite image enhancement; manual verification of all values required"})
    # Ambiguous classification: the winning type barely beat the runner-up.
    if ctx.get("classify_ambiguous") and ctx.get("doc_type") != "unrelated":
        findings.append({"field": None, "issue": f"Document type '{ctx['doc_type']}' is a low-margin classification — confirm type on review"})
    # Hallucination guard from verify().
    for field in ctx.get("ungrounded_fields", []):
        findings.append({"field": field, "issue": "Extracted value not found in document text — possible extraction error; verify before use"})
    # Shape warnings from normalize().
    for w in ctx.get("normalize_warnings", []):
        findings.append({"field": w["field"], "issue": w["issue"]})
    ctx["findings"] = findings
    ctx["status"] = "ANALYZED" if not findings else "ANALYZED_WITH_FINDINGS"
    return ctx


def stage_index(ctx: dict, cfg: dict) -> dict:
    """Build the OpenSearch document body. main.persist() performs the actual
    index call (it owns the client); this stage decides WHAT is searchable:
    full text, trusted normalized fields, doc type, and confidence map."""
    ctx["index_body"] = {
        "doc_type": ctx.get("doc_type"),
        "text": ctx.get("text", "")[:100000],
        "extracted": ctx.get("extracted", {}),
        "normalized": ctx.get("normalized", {}),
        "field_confidence": ctx.get("field_confidence", {}),
        "status": ctx.get("status"),
    }
    return ctx


# ---------------------------------------------------------------------------
# Docking engine
# ---------------------------------------------------------------------------

_ENV_VAR_RE = re.compile(r"\$\{(\w+)(:-([^}]*))?\}")


def _expand_env_vars(text: str) -> str:
    """${VAR:-default} / ${VAR} substitution -- PyYAML does not do shell-style
    env-var expansion on its own, so pipeline.yaml's endpoint/model defaults
    (e.g. ${VLM_ENDPOINT:-http://vllm:8000/v1}) were being passed through
    verbatim as literal strings. Confirmed live: the VLM client was trying to
    connect to the literal unexpanded string as a URL, surfacing as a generic
    "Connection error" with no hint the config was never resolved."""
    def repl(m: re.Match) -> str:
        name, _, default = m.groups()
        return os.environ.get(name, default or "")
    return _ENV_VAR_RE.sub(repl, text)


_PIPELINE_CACHE: dict[str, tuple[float, dict]] = {}


def load_pipeline(path: str = "pipeline.yaml") -> dict:
    """YAML spec, cached by mtime — run_pipeline() is called per document,
    and re-reading + re-parsing + env-expanding the spec each time is pure
    overhead. An edit to pipeline.yaml invalidates via mtime."""
    mtime = os.path.getmtime(path)
    cached = _PIPELINE_CACHE.get(path)
    if cached and cached[0] == mtime:
        return cached[1]
    with open(path) as f:
        spec = yaml.safe_load(_expand_env_vars(f.read()))
    _PIPELINE_CACHE[path] = (mtime, spec)
    return spec


def run_pipeline(ctx: dict, case: dict | None = None, schema_overrides: dict | None = None) -> dict:
    spec = load_pipeline()
    # Tenants may declare their own extraction schemas (NG: Program Manifest
    # documents.schemas). Overrides MERGE onto the built-ins — a tenant
    # schema shadows the same-named built-in, unknown names just extend.
    schemas = {**spec["schemas"], **(schema_overrides or {})}
    for st in spec["stages"]:
        if not st.get("enabled", True):
            continue
        # Conditional docking: a stage runs only when its `when` flag is truthy
        # in the pipeline context (e.g. PaddleOCR only when Docling found a scan).
        cond = st.get("when")
        if cond and not ctx.get(cond):
            continue
        name, cfg = st["name"], st.get("config", {})
        try:
            if name == "vlm_extract":
                ctx = stage_vlm_extract(ctx, cfg, schemas)
            elif name == "validate":
                ctx = stage_validate(ctx, cfg, case)
            else:
                fn = globals().get(f"stage_{name}")
                if fn is None:
                    raise RuntimeError(f"pipeline stage not implemented: {name}")
                ctx = fn(ctx, cfg)
        except Exception as exc:
            # docling is the primary parser -- if it fails there is no usable
            # text/markdown/tables at all, so that failure stays fatal (same
            # as before). Every other stage (ocr, layout/seal-detection,
            # vlm_extract) is an enhancement on top of what docling already
            # extracted: a missing vLLM endpoint or a PaddleX dependency
            # issue shouldn't take the whole document down with it. Confirmed
            # live: layout's PP-StructureV3 construction failing aborted
            # run_pipeline() before persist() or the DOCS_VERIFIED/
            # DOC_ANALYZED signal ever ran -- for every document, forever,
            # not just the one that happened to trigger it first.
            if name == "docling":
                raise
            ctx.setdefault("findings", []).append(
                {"stage": name, "issue": f"stage failed, skipped: {exc}"}
            )
    ctx.setdefault("status", "ANALYZED")
    return ctx


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def np_from_pil(img: Image.Image):
    import numpy as np
    return np.array(img.convert("RGB"))


def pil_to_b64(img: Image.Image) -> str:
    """JPEG, not PNG: a 300dpi page PNG is 5-10x the bytes of a q85 JPEG at
    no loss the VLM can detect, and the payload rides base64 into the prompt
    of the LOCAL Ollama — smaller payload = faster encode, faster transfer,
    less memory on a shared instance with an OOM history."""
    import base64
    buf = io.BytesIO()
    img.convert("RGB").save(buf, format="JPEG", quality=85)
    return base64.b64encode(buf.getvalue()).decode()


def pdf_to_pages(data: bytes, dpi: int = 200,
                 max_pages: int | None = None) -> tuple[list[Image.Image], bool]:
    """Rasterize PDF pages (pypdfium2 bundled with PaddleOCR toolchain).

    Page-capped: each page becomes a full-resolution bitmap in worker memory,
    so an unbounded render of a huge/hostile PDF is a memory bomb that kills
    the consumer for every tenant. Returns (pages, truncated)."""
    import pypdfium2 as pdfium

    cap = max_pages or int(os.environ.get("DOC_INTEL_MAX_PAGES", "150"))
    pdf = pdfium.PdfDocument(data)
    pages = []
    truncated = False
    for i, page in enumerate(pdf):
        if i >= cap:
            truncated = True
            break
        pages.append(page.render(scale=dpi / 72).to_pil())
    return pages, truncated


# OOXML safety: a tiny .docx can expand to gigabytes when parsed. Before
# Docling opens an office container, sum the UNCOMPRESSED sizes from the zip
# central directory (no decompression needed) and reject beyond the cap.
OOXML_MAX_EXPANDED_BYTES = 512 * 1024 * 1024   # 512 MiB expanded ceiling
OOXML_MAX_RATIO = 200                           # compressed:expanded sanity bound


def ooxml_safe(data: bytes) -> tuple[bool, str]:
    """(safe, reason). Reads central directory metadata only — never inflates."""
    import zipfile
    try:
        zf = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile:
        return False, "office container is corrupt or not a valid zip"
    total = sum(i.file_size for i in zf.infolist())
    if total > OOXML_MAX_EXPANDED_BYTES:
        return False, f"expands to {total >> 20} MiB — exceeds the {OOXML_MAX_EXPANDED_BYTES >> 20} MiB safety cap"
    if len(data) > 0 and total / len(data) > OOXML_MAX_RATIO:
        return False, f"expansion ratio {total // max(len(data),1)}:1 — probable zip bomb"
    return True, ""


def sniff_doc_kind(data: bytes, content_type: str) -> str:
    """Cheap magic-byte gate run BEFORE any parsing: pdf / image / office /
    text / unknown. The API edge enforces an allowlist, but events can also
    arrive from paths that predate it — defense in depth, and a clean
    UNSUPPORTED_TYPE instead of an exception-marked ERROR."""
    if data[:5] == b"%PDF-":
        return "pdf"
    if data[:4] == b"PK\x03\x04":
        return "office"
    if (data[:8] == b"\x89PNG\r\n\x1a\n" or data[:3] == b"\xff\xd8\xff"
            or data[:4] in (b"II*\x00", b"MM\x00*", b"GIF8") or data[:2] == b"BM"
            or (data[:4] == b"RIFF" and data[8:12] == b"WEBP")):
        return "image"
    sample = data[:4096]
    if sample and b"\x00" not in sample:
        printable = sum(1 for b in sample if b in b"\t\n\r" or 0x20 <= b < 0x7F or b >= 0x80)
        if printable / len(sample) > 0.95:
            return "text"
    return "unknown"


def ensure_pages(ctx: dict, dpi: int = 200) -> list[Image.Image]:
    """Lazily rasterize PDF pages. Only stages that truly need bitmaps
    (OCR fallback, vision VLM, layout analysis) pay the render cost —
    previously EVERY PDF was rasterized at 200dpi before Docling even
    ran, and for born-digital documents (the common case) those bitmaps
    were never used: seconds of CPU and hundreds of MB per doc, wasted."""
    if ctx.get("pages"):
        return ctx["pages"]
    if sniff_doc_kind(ctx.get("raw_bytes", b""), ctx.get("content_type", "")) != "pdf":
        return ctx.get("pages") or []
    pages, truncated = pdf_to_pages(ctx["raw_bytes"], dpi=dpi)
    ctx["pages"] = pages
    if truncated:
        ctx["pages_truncated"] = True
    return pages


def bytes_to_pages(data: bytes, content_type: str,
                   kind: str | None = None) -> tuple[list[Image.Image], bool]:
    """(pages, truncated). kind comes from sniff_doc_kind when available."""
    k = kind or ("pdf" if "pdf" in content_type else "image")
    if k == "pdf":
        return pdf_to_pages(data)
    if k == "image":
        return [Image.open(io.BytesIO(data))], False
    return [], False  # office/text: Docling reads raw bytes; no raster pages
